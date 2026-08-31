//go:build windows

// Auto-update for the tray app, in the order a user experiences it:
//
//  1. On launch, an update staged by a previous session is installed before the
//     proxy starts and the installer relaunches the tray. Otherwise the tray
//     spends at most updateCheckWindow deciding whether a newer release exists;
//     no answer in time, or nothing newer, and the proxy starts immediately. If
//     one is found the app stays down until it is installed, so the window
//     bounds the decision, not the work.
//  2. While running, an hourly poller downloads and verifies a newer release and
//     flags it for next startup, revealing "Restart to Update" to take it now.
//  3. On Exit a flagged update is installed on the way out.
//
// Nothing is executed unverified: the asset must match the release's
// SHA256SUMS.txt and carry a DOS header. The installer replaces executables
// only, so tokens.sqlite, logs/ and .env survive.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	updateRepo         = "jubinjacob03/glm-proxy"
	updateAssetName    = "GLM-Proxy-Setup.exe"
	updateChecksumName = "SHA256SUMS.txt"

	updateCheckInterval   = 60 * time.Minute
	updateStartupDelay    = 90 * time.Second
	updateHTTPTimeout     = 15 * time.Minute
	updateChecksumTimeout = 30 * time.Second
	updateMaxAssetBytes   = 200 << 20

	// All a launch spends deciding whether an update exists. Measured at ~0.4s
	// in practice, so this is a ceiling for slow links, not a target.
	updateCheckWindow = 6 * time.Second

	// Bounds the download and install once one has been found. The app stays
	// down for this, so it is generous rather than snappy.
	updateApplyBudget = 15 * time.Minute

	// Stops a staged installer that never raises the installed version from
	// being retried on every launch.
	updateMaxAttempts = 2

	updateStagingDir = "update-staging"
	updateMarkerName = "pending.json"

	defaultUpdateAPIBase = "https://api.github.com"
	defaultUpdateWebBase = "https://github.com"
)

// appVersion is stamped at build time via -ldflags "-X main.appVersion=1.2.3".
// An unstamped build keeps the "dev" sentinel and never self-updates, so a local
// build is never replaced by a published release.
var appVersion = "dev"

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

// pendingUpdate is the on-disk flag for a verified installer awaiting apply. It
// survives restarts, which is what lets an update found while running be
// installed at the next launch.
type pendingUpdate struct {
	Version  string `json:"version"`
	File     string `json:"file"`
	SHA256   string `json:"sha256"`
	Attempts int    `json:"attempts"`
}

type updater struct {
	stagingDir string

	// client carries the multi-megabyte installer download.
	client *http.Client
	// metaClient has short dial/handshake/header budgets for the launch-critical
	// check, so a wedged network fails fast.
	metaClient *http.Client
	// tagClient is metaClient without redirect following, so the release page's
	// 302 can be read rather than followed.
	tagClient *http.Client

	apiBase string
	webBase string

	mu            sync.Mutex
	stagedVersion string
	stagedPath    string

	// Runs once per newly staged version, on the poller goroutine.
	onStaged func(version string)

	// Reports launch progress to the splash. Unset for background polling, which
	// has no window to draw on.
	onStatus func(percent int, text string)
}

// status pushes a line to the splash when one is attached.
func (u *updater) status(percent int, text string) {
	if u.onStatus != nil {
		u.onStatus(percent, text)
	}
}

func newUpdater(installDir string, onStaged func(version string)) *updater {
	// Shared by both metadata clients, so an unreachable host cannot eat the
	// whole check window waiting on a socket.
	metaTransport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 4 * time.Second}).DialContext,
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	}
	noFollow := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &updater{
		stagingDir: filepath.Join(installDir, updateStagingDir),
		client:     &http.Client{Timeout: updateHTTPTimeout},
		metaClient: &http.Client{Transport: metaTransport, Timeout: updateCheckWindow},
		tagClient:  &http.Client{Transport: metaTransport, Timeout: updateCheckWindow, CheckRedirect: noFollow},
		apiBase:    updateAPIBase(),
		webBase:    updateWebBase(),
		onStaged:   onStaged,
	}
}

// updateAPIBase redirects the release feed for local testing. Honoured only for
// loopback, so it cannot aim a real install at a hostile feed.
func updateAPIBase() string {
	v := strings.TrimSpace(os.Getenv("GLM_UPDATE_API"))
	if v == "" {
		return defaultUpdateAPIBase
	}
	if !isLoopbackURL(v) {
		return defaultUpdateAPIBase
	}
	return strings.TrimRight(v, "/")
}

// updateWebBase mirrors updateAPIBase for the release-page host used by the tag
// lookup, so one loopback override points both at the same test server.
func updateWebBase() string {
	v := strings.TrimSpace(os.Getenv("GLM_UPDATE_API"))
	if v == "" || !isLoopbackURL(v) {
		return defaultUpdateWebBase
	}
	return strings.TrimRight(v, "/")
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// updateInterval is the poll period, overridable for tests.
func updateInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("GLM_UPDATE_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 5*time.Second {
			return d
		}
	}
	return updateCheckInterval
}

// updateFirstDelay keeps the first poll clear of startup, shortening itself when
// a small interval signals a test run.
func updateFirstDelay() time.Duration {
	if iv := updateInterval(); iv < 5*time.Minute {
		return 5 * time.Second
	}
	return updateStartupDelay
}

// autoUpdateEnabled reports whether updating happens at all; AUTO_UPDATE=false
// in .env turns it off entirely.
func autoUpdateEnabled(envPath string) bool {
	switch strings.ToLower(strings.TrimSpace(getEnvValue(envPath, "AUTO_UPDATE"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// updatable reports whether this build may be replaced at all.
func updatable() bool {
	return parseVersion(appVersion) != nil
}

// ── pending flag ─────────────────────────────────────────────────────────────

func (u *updater) markerPath() string {
	return filepath.Join(u.stagingDir, updateMarkerName)
}

func (u *updater) readPending() (*pendingUpdate, bool) {
	data, err := os.ReadFile(u.markerPath())
	if err != nil {
		return nil, false
	}
	var p pendingUpdate
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, false
	}
	if p.Version == "" || p.File == "" {
		return nil, false
	}
	return &p, true
}

func (u *updater) writePending(p *pendingUpdate) error {
	if err := os.MkdirAll(u.stagingDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(u.markerPath(), data, 0o644)
}

func (u *updater) clearPending() {
	_ = os.Remove(u.markerPath())
}

// pendingInstaller is the single gate on a startup update: the flagged version
// must be newer than this build and its file must still be present.
func (u *updater) pendingInstaller() (*pendingUpdate, string, bool) {
	p, ok := u.readPending()
	if !ok {
		return nil, "", false
	}
	if compareVersions(parseVersion(p.Version), parseVersion(appVersion)) <= 0 {
		return p, "", false
	}
	path := filepath.Join(u.stagingDir, p.File)
	if _, err := os.Stat(path); err != nil {
		return p, "", false
	}
	return p, path, true
}

// ── startup path ─────────────────────────────────────────────────────────────

// applyStartupUpdate runs before the proxy starts. True means an installer was
// launched, so the caller must exit and let it replace the files and relaunch.
func (u *updater) applyStartupUpdate(ctx context.Context) bool {
	pending, pendingPath, pendingReady := u.pendingInstaller()
	if !pendingReady && pending != nil {
		logUpdate("clearing stale update flag for %s (running %s)", pending.Version, appVersion)
		u.clearPending()
		u.pruneStaging("")
		return false
	}

	if pendingReady {
		p, path := pending, pendingPath
		if p.Attempts >= updateMaxAttempts {
			logUpdate("update to %s failed %d times; abandoning it", p.Version, p.Attempts)
			u.clearPending()
			u.pruneStaging("")
			return false
		}
		p.Attempts++
		if err := u.writePending(p); err != nil {
			logUpdate("could not record update attempt: %v", err)
		}
		u.setStaged(p.Version, path)
		logUpdate("installing flagged update %s before startup (attempt %d)", p.Version, p.Attempts)

		u.status(splashIndeterminate, "Installing update v"+p.Version+"...")
		if err := u.applyNow(true); err != nil {
			logUpdate("startup install failed: %v", err)
			u.status(splashIndeterminate, "Starting GLM Proxy...")
			return false
		}
		return true
	}

	decisionCtx, cancelDecision := context.WithTimeout(ctx, updateCheckWindow)
	defer cancelDecision()
	started := time.Now()
	tag, err := u.latestTag(decisionCtx)
	if err != nil {
		logUpdate("version check gave up after %v (%v); starting normally",
			time.Since(started).Round(time.Millisecond), err)
		return false
	}
	if compareVersions(parseVersion(tag), parseVersion(appVersion)) <= 0 {
		logUpdate("up to date at %s (checked in %v)",
			appVersion, time.Since(started).Round(time.Millisecond))
		return false
	}

	// One exists, so the app stays down until it is installed: the window above
	// bounded the decision, not this.
	version := normalizeVersion(tag)
	rel, err := u.fetchLatest(decisionCtx)
	if err != nil {
		logUpdate("update %s found but its details could not be read (%v); starting without it",
			version, err)
		return false
	}
	if !releaseIsNewer(rel) {
		return false
	}
	logUpdate("update %s found; holding startup until it is installed", version)

	u.status(0, "Downloading update v"+version+"...")
	applyCtx, cancel := context.WithTimeout(ctx, updateApplyBudget)
	defer cancel()

	if err := u.stageRelease(applyCtx, rel); err != nil {
		logUpdate("update %s could not be prepared (%v); starting without it", version, err)
		u.status(splashIndeterminate, "Starting GLM Proxy...")
		return false
	}
	if _, _, ok := u.staged(); !ok {
		u.status(splashIndeterminate, "Starting GLM Proxy...")
		return false
	}
	logUpdate("installing freshly downloaded update %s before startup", version)
	u.status(splashIndeterminate, "Installing update v"+version+"...")
	if err := u.applyNow(true); err != nil {
		logUpdate("startup install failed: %v", err)
		u.status(splashIndeterminate, "Starting GLM Proxy...")
		return false
	}
	return true
}

// ── polling ──────────────────────────────────────────────────────────────────

// run polls on the configured interval until ctx is cancelled.
func (u *updater) run(ctx context.Context) {
	if !updatable() {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(updateFirstDelay()):
	}

	interval := updateInterval()
	for {
		if err := u.checkOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logUpdate("check failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// checkOnce decides cheaply, then works only if it must: the tag lookup is one
// bodiless redirect, and the 11 KB rate-limited JSON is fetched only once a newer
// version is known to exist.
func (u *updater) checkOnce(ctx context.Context) error {
	decisionCtx, cancel := context.WithTimeout(ctx, updateCheckWindow)
	if tag, err := u.latestTag(decisionCtx); err == nil {
		if compareVersions(parseVersion(tag), parseVersion(appVersion)) <= 0 {
			cancel()
			return nil
		}
	}
	rel, err := u.fetchLatest(decisionCtx)
	cancel()
	if err != nil {
		return err
	}
	return u.stageRelease(ctx, rel)
}

// latestTag reads the newest tag out of a redirect: /releases/latest answers 302
// with Location .../releases/tag/<tag>, so the answer arrives in a header.
// Measured at about a third of the JSON API's latency, zero body, no rate limit.
func (u *updater) latestTag(ctx context.Context) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, updateCheckWindow)
	defer cancel()

	endpoint := u.webBase + "/" + updateRepo + "/releases/latest"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "glm-tray/"+appVersion)
	req.Header.Set("Accept", "text/html")

	resp, err := u.tagClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("release page returned %d, expected a redirect", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("redirect carried no Location header")
	}
	tag := loc
	if i := strings.LastIndex(loc, "/"); i >= 0 {
		tag = loc[i+1:]
	}
	if parseVersion(tag) == nil {
		return "", fmt.Errorf("unusable tag %q in redirect", tag)
	}
	return tag, nil
}

// fetchLatest resolves the newest published release inside the check window.
func (u *updater) fetchLatest(ctx context.Context) (*ghRelease, error) {
	metaCtx, cancel := context.WithTimeout(ctx, updateCheckWindow)
	defer cancel()
	return u.latestRelease(metaCtx)
}

// releaseIsNewer reports whether rel is published and worth installing.
func releaseIsNewer(rel *ghRelease) bool {
	if rel == nil || rel.Draft || rel.Prerelease {
		return false
	}
	remote := parseVersion(rel.TagName)
	if remote == nil {
		return false
	}
	return compareVersions(remote, parseVersion(appVersion)) > 0
}

// stageRelease downloads and flags rel if it is newer than the running build.
func (u *updater) stageRelease(ctx context.Context, rel *ghRelease) error {
	if rel.Draft || rel.Prerelease {
		return nil
	}

	remote := parseVersion(rel.TagName)
	if remote == nil {
		return fmt.Errorf("unparsable release tag %q", rel.TagName)
	}
	if compareVersions(remote, parseVersion(appVersion)) <= 0 {
		return nil
	}
	version := normalizeVersion(rel.TagName)

	if pending, path, ok := u.pendingInstaller(); ok && pending.Version == version {
		u.setStaged(version, path)
		return nil
	}

	asset, ok := findAsset(rel.Assets, updateAssetName)
	if !ok {
		return fmt.Errorf("release %s has no %s asset", rel.TagName, updateAssetName)
	}
	if asset.Size > updateMaxAssetBytes {
		return fmt.Errorf("release asset is too large: %d bytes exceeds %d", asset.Size, updateMaxAssetBytes)
	}

	stageCtx, cancel := context.WithTimeout(ctx, updateApplyBudget)
	defer cancel()

	logUpdate("release %s is newer than %s; downloading %s (%d bytes)",
		rel.TagName, appVersion, asset.Name, asset.Size)

	wantSum, err := u.expectedChecksum(stageCtx, rel, asset.Name)
	if err != nil {
		return err
	}
	path, err := u.download(stageCtx, version, asset, wantSum)
	if err != nil {
		return err
	}

	u.setStaged(version, path)
	if err := u.writePending(&pendingUpdate{
		Version: version,
		File:    filepath.Base(path),
		SHA256:  wantSum,
	}); err != nil {
		logUpdate("could not write update flag: %v", err)
	}
	u.pruneStaging(filepath.Base(path))
	logUpdate("staged %s at %s and flagged for install", version, path)

	if u.onStaged != nil {
		u.onStaged(version)
	}
	return nil
}

func (u *updater) latestRelease(ctx context.Context) (*ghRelease, error) {
	endpoint := u.apiBase + "/repos/" + updateRepo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "glm-tray/"+appVersion)

	resp, err := u.metaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github returned %d", resp.StatusCode)
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, errors.New("release has no tag")
	}
	return &rel, nil
}

// expectedChecksum returns the digest SHA256SUMS.txt records for name. A release
// without that asset is refused rather than installed unverified.
func (u *updater) expectedChecksum(ctx context.Context, rel *ghRelease, name string) (string, error) {
	sums, ok := findAsset(rel.Assets, updateChecksumName)
	if !ok {
		return "", fmt.Errorf("release %s has no %s; refusing to install unverified",
			rel.TagName, updateChecksumName)
	}
	checksumCtx, cancel := context.WithTimeout(ctx, updateChecksumTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checksumCtx, http.MethodGet, sums.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "glm-tray/"+appVersion)
	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksum download returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	sum, ok := parseChecksums(string(data), name)
	if !ok {
		return "", fmt.Errorf("%s has no entry for %s", updateChecksumName, name)
	}
	return sum, nil
}

// download streams to a .part file, hashing as it goes, and promotes it only once
// the digest matches and the payload looks like a Windows executable.
func (u *updater) download(ctx context.Context, version string, asset ghAsset, wantSum string) (string, error) {
	if err := os.MkdirAll(u.stagingDir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(u.stagingDir, "GLM-Proxy-Setup-"+sanitizeFileToken(version)+".exe")
	part := final + ".part"
	_ = os.Remove(part)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "glm-tray/"+appVersion)

	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("asset download returned %d", resp.StatusCode)
	}

	f, err := os.Create(part)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	writers := []io.Writer{f, hasher}
	if u.onStatus != nil {
		writers = append(writers, &progressMeter{
			total: asset.Size, label: "Downloading update v" + version, report: u.onStatus,
		})
	}
	written, err := io.Copy(io.MultiWriter(writers...), io.LimitReader(resp.Body, updateMaxAssetBytes+1))
	closeErr := f.Close()
	if err != nil {
		os.Remove(part)
		return "", err
	}
	if closeErr != nil {
		os.Remove(part)
		return "", closeErr
	}
	if written > updateMaxAssetBytes {
		os.Remove(part)
		return "", fmt.Errorf("asset exceeds %d bytes", updateMaxAssetBytes)
	}
	if asset.Size > 0 && written != asset.Size {
		os.Remove(part)
		return "", fmt.Errorf("size mismatch: got %d, want %d", written, asset.Size)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, wantSum) {
		os.Remove(part)
		return "", fmt.Errorf("checksum mismatch: got %s, want %s", got, wantSum)
	}
	if err := verifyPEFile(part); err != nil {
		os.Remove(part)
		return "", err
	}

	_ = os.Remove(final)
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		return "", err
	}
	return final, nil
}

// progressMeter throttles splash updates while always reporting completion.
type progressMeter struct {
	total  int64
	label  string
	report func(percent int, text string)

	written int64
	lastAt  time.Time
	lastPct int
}

func (m *progressMeter) Write(p []byte) (int, error) {
	n := len(p)
	m.written += int64(n)
	if m.report == nil {
		return n, nil
	}

	percent := splashIndeterminate
	if m.total > 0 {
		percent = int(m.written * 100 / m.total)
		if percent > 100 {
			percent = 100
		}
	}
	done := m.total > 0 && m.written >= m.total
	now := time.Now()
	if done && m.lastPct == 100 {
		return n, nil
	}
	if !done && !m.lastAt.IsZero() && now.Sub(m.lastAt) < 200*time.Millisecond {
		return n, nil
	}
	m.lastAt, m.lastPct = now, percent

	text := m.label + "..."
	if m.total > 0 {
		displayed := min(m.written, m.total)
		text = fmt.Sprintf("%s  %d%%  (%s of %s)", m.label, percent, humanMB(displayed), humanMB(m.total))
	}
	m.report(percent, text)
	return n, nil
}

// pruneStaging deletes staged files other than keep, leaving the flag intact.
func (u *updater) pruneStaging(keep string) {
	entries, err := os.ReadDir(u.stagingDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == keep || e.Name() == updateMarkerName {
			continue
		}
		_ = os.Remove(filepath.Join(u.stagingDir, e.Name()))
	}
}

func (u *updater) setStaged(version, path string) {
	u.mu.Lock()
	u.stagedVersion, u.stagedPath = version, path
	u.mu.Unlock()
}

// staged returns the pending version and its installer path, if any.
func (u *updater) staged() (string, string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stagedPath == "" {
		return "", "", false
	}
	return u.stagedVersion, u.stagedPath, true
}

// applyNow runs the staged installer. relaunch has it restart the tray when it
// finishes, which the startup path and "Restart to Update" both want; Exit leaves
// it false and relies on autostart.
func (u *updater) applyNow(relaunch bool) error {
	version, path, ok := u.staged()
	if !ok {
		return errors.New("no update staged")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("staged installer missing: %w", err)
	}
	logUpdate("applying %s from %s (relaunch=%v)", version, path, relaunch)
	return launchInstaller(path, relaunch)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// verifyPEFile checks the DOS header, so an HTML error page or truncated body is
// never handed to the shell as an executable.
func verifyPEFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("cannot read header: %w", err)
	}
	if magic[0] != 'M' || magic[1] != 'Z' {
		return errors.New("downloaded file is not a Windows executable")
	}
	return nil
}

func findAsset(assets []ghAsset, name string) (ghAsset, bool) {
	for _, a := range assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return ghAsset{}, false
}

// parseChecksums reads sha256sum-style "<hex>  <name>" lines for want's digest.
func parseChecksums(data, want string) (string, bool) {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if strings.EqualFold(filepath.Base(name), want) {
			sum := fields[0]
			if len(sum) != 64 {
				return "", false
			}
			if _, err := hex.DecodeString(sum); err != nil {
				return "", false
			}
			return sum, true
		}
	}
	return "", false
}

// normalizeVersion drops a leading "v" and build metadata, so "v1.2.3" and
// "1.2.3" compare equal.
func normalizeVersion(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexAny(s, "+"); i >= 0 {
		s = s[:i]
	}
	return s
}

// parseVersion turns a dotted numeric version into comparable parts, returning
// nil for anything that is not a release — the "dev" sentinel and pre-release
// spellings included, which keeps those builds out of the update path.
func parseVersion(s string) []int {
	s = normalizeVersion(s)
	if s == "" || strings.ContainsAny(s, "-") {
		return nil
	}
	fields := strings.Split(s, ".")
	if len(fields) == 0 || len(fields) > 4 {
		return nil
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			return nil
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// compareVersions returns -1, 0 or 1. An unparsable version sorts lowest, so a
// dev build is never newer than a release.
func compareVersions(a, b []int) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// sanitizeFileToken makes a version or asset name safe to join onto a path.
func sanitizeFileToken(s string) string {
	s = filepath.Base(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "asset"
	}
	return out
}
