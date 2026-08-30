//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestParseVersionRejectsNonReleases(t *testing.T) {
	for _, s := range []string{"dev", "", "1.2.3-rc1", "v1.2.3-beta", "abc", "1.a.3", "1.2.3.4.5", "-1.0"} {
		if got := parseVersion(s); got != nil {
			t.Errorf("parseVersion(%q) = %v, want nil", s, got)
		}
	}
	for _, s := range []string{"1", "1.0", "1.2.3", "v1.2.3", "V1.2.3", "1.2.3+build7", " 1.2.3 "} {
		if got := parseVersion(s); got == nil {
			t.Errorf("parseVersion(%q) = nil, want parsed", s)
		}
	}
}

func TestCompareVersionsOrdering(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"v1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.0", "1.0.0", 0},
		{"1.0.1", "1.0", 1},
		{"1.2.3.1", "1.2.3", 1},
	}
	for _, c := range cases {
		got := compareVersions(parseVersion(c.a), parseVersion(c.b))
		if got != c.want {
			t.Errorf("compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	// A dev build must not look newer than a release, in either direction.
	if compareVersions(parseVersion("dev"), parseVersion("1.0.0")) != -1 {
		t.Error("dev should sort below a release")
	}
	if compareVersions(parseVersion("1.0.0"), parseVersion("dev")) != 1 {
		t.Error("a release should sort above dev")
	}
}

func TestParseChecksumsFindsEntry(t *testing.T) {
	data := "# generated\n" +
		"aa" + repeat("0", 62) + "  zai-api.exe\n" +
		"bb" + repeat("1", 62) + "  GLM-Proxy-Setup.exe\n"

	sum, ok := parseChecksums(data, "GLM-Proxy-Setup.exe")
	if !ok {
		t.Fatal("expected to find the setup entry")
	}
	if sum != "bb"+repeat("1", 62) {
		t.Errorf("wrong digest: %s", sum)
	}
	if _, ok := parseChecksums(data, "missing.exe"); ok {
		t.Error("unknown name should not match")
	}
	// A malformed digest is refused, never trusted.
	if _, ok := parseChecksums("zzzz  GLM-Proxy-Setup.exe\n", "GLM-Proxy-Setup.exe"); ok {
		t.Error("short digest should be rejected")
	}
	if _, ok := parseChecksums("gggg"+repeat("g", 60)+"  GLM-Proxy-Setup.exe\n", "GLM-Proxy-Setup.exe"); ok {
		t.Error("non-hex digest should be rejected")
	}
	// sha256sum prefixes binary entries with '*', which must not break parsing.
	if _, ok := parseChecksums(repeat("a", 64)+" *GLM-Proxy-Setup.exe\n", "GLM-Proxy-Setup.exe"); !ok {
		t.Error("binary marker (*) should still match")
	}
}

func TestVerifyPEFileRejectsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	html := filepath.Join(dir, "err.exe")
	os.WriteFile(html, []byte("<html>404</html>"), 0o644)
	if err := verifyPEFile(html); err == nil {
		t.Error("an HTML error page must not pass as an executable")
	}

	pe := filepath.Join(dir, "ok.exe")
	os.WriteFile(pe, []byte("MZ\x90\x00rest"), 0o644)
	if err := verifyPEFile(pe); err != nil {
		t.Errorf("valid DOS header rejected: %v", err)
	}
}

func TestAutoUpdateEnabledOptOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	if !autoUpdateEnabled(filepath.Join(dir, "absent.env")) {
		t.Error("a missing .env should leave auto-update on")
	}
	for _, v := range []string{"false", "FALSE", "0", "no", "off", " off "} {
		os.WriteFile(path, []byte("AUTO_UPDATE="+v+"\n"), 0o600)
		if autoUpdateEnabled(path) {
			t.Errorf("AUTO_UPDATE=%q should disable auto-update", v)
		}
	}
	for _, v := range []string{"true", "1", "on", ""} {
		os.WriteFile(path, []byte("AUTO_UPDATE="+v+"\n"), 0o600)
		if !autoUpdateEnabled(path) {
			t.Errorf("AUTO_UPDATE=%q should leave auto-update on", v)
		}
	}
}

func TestGetEnvValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	os.WriteFile(path, []byte("# comment\nexport ZAI_TOKEN=\"quoted.jwt\"\nPORT=3007\nAUTO_UPDATE=false\n"), 0o600)

	if got := getEnvValue(path, "ZAI_TOKEN"); got != "quoted.jwt" {
		t.Errorf("quotes and export prefix not handled: %q", got)
	}
	if got := getEnvValue(path, "PORT"); got != "3007" {
		t.Errorf("PORT = %q", got)
	}
	if got := getEnvValue(path, "NOPE"); got != "" {
		t.Errorf("absent key should be empty, got %q", got)
	}
}

func TestStagedReportsNothingByDefault(t *testing.T) {
	u := newUpdater(t.TempDir(), nil)
	if _, _, ok := u.staged(); ok {
		t.Error("a fresh updater must report no staged update")
	}
	if err := u.applyNow(false); err == nil {
		t.Error("applyNow with nothing staged should fail")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// newFakeRelease serves a GitHub-shaped release and its assets, so the whole
// download-verify-stage path runs without touching the network.
func newFakeRelease(t *testing.T, tag string, payload []byte, sum string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/setup.exe", func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	})
	mux.HandleFunc("/sums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", sum, updateAssetName)
	})
	mux.HandleFunc("/repos/"+updateRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		rel := ghRelease{
			TagName: tag,
			Assets: []ghAsset{
				{Name: updateAssetName, URL: srv.URL + "/setup.exe", Size: int64(len(payload))},
				{Name: updateChecksumName, URL: srv.URL + "/sums.txt"},
			},
		}
		json.NewEncoder(w).Encode(rel)
	})
	// latestTag reads the tag from this redirect's Location header, so the mock
	// must serve it or the lookup escapes to real github.com.
	mux.HandleFunc("/"+updateRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/"+updateRepo+"/releases/tag/"+tag)
		w.WriteHeader(http.StatusFound)
	})
	return srv
}

func TestCheckOnceStagesVerifiedUpdate(t *testing.T) {
	orig := appVersion
	appVersion = "1.0.0"
	defer func() { appVersion = orig }()

	payload := []byte("MZ\x90\x00 pretend installer body")
	digest := sha256.Sum256(payload)
	sum := hex.EncodeToString(digest[:])

	srv := newFakeRelease(t, "v1.2.0", payload, sum)

	var notified string
	u := newUpdater(t.TempDir(), func(v string) { notified = v })
	u.apiBase = srv.URL
	u.webBase = srv.URL

	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	version, path, ok := u.staged()
	if !ok {
		t.Fatal("expected an update to be staged")
	}
	if version != "1.2.0" {
		t.Errorf("staged version = %q, want 1.2.0", version)
	}
	if notified != "1.2.0" {
		t.Errorf("onStaged callback got %q", notified)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("staged file unreadable: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("staged bytes differ from served payload")
	}
	// Promotion must leave no .part file behind.
	entries, _ := os.ReadDir(u.stagingDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".part" {
			t.Errorf("leftover partial download: %s", e.Name())
		}
	}
}

func TestCheckOnceRejectsBadChecksum(t *testing.T) {
	orig := appVersion
	appVersion = "1.0.0"
	defer func() { appVersion = orig }()

	payload := []byte("MZ\x90\x00 tampered body")
	srv := newFakeRelease(t, "v1.2.0", payload, repeat("a", 64))

	u := newUpdater(t.TempDir(), nil)
	u.apiBase = srv.URL
	u.webBase = srv.URL

	err := u.checkOnce(context.Background())
	if err == nil {
		t.Fatal("a checksum mismatch must fail the check")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, _, ok := u.staged(); ok {
		t.Error("nothing may be staged after a checksum mismatch")
	}
	entries, _ := os.ReadDir(u.stagingDir)
	if len(entries) != 0 {
		t.Errorf("failed download left files behind: %d", len(entries))
	}
}

func TestCheckOnceIgnoresSameOrOlderRelease(t *testing.T) {
	orig := appVersion
	appVersion = "2.0.0"
	defer func() { appVersion = orig }()

	payload := []byte("MZ\x90\x00 body")
	digest := sha256.Sum256(payload)
	srv := newFakeRelease(t, "v1.0.0", payload, hex.EncodeToString(digest[:]))

	u := newUpdater(t.TempDir(), func(string) { t.Error("callback must not fire for an older release") })
	u.apiBase = srv.URL
	u.webBase = srv.URL

	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if _, _, ok := u.staged(); ok {
		t.Error("an older release must not be staged")
	}
}

func TestCheckOnceRefusesReleaseWithoutChecksums(t *testing.T) {
	orig := appVersion
	appVersion = "1.0.0"
	defer func() { appVersion = orig }()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/repos/"+updateRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ghRelease{
			TagName: "v1.5.0",
			Assets:  []ghAsset{{Name: updateAssetName, URL: srv.URL + "/setup.exe", Size: 4}},
		})
	})

	u := newUpdater(t.TempDir(), nil)
	u.apiBase = srv.URL
	u.webBase = srv.URL

	err := u.checkOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), updateChecksumName) {
		t.Fatalf("expected refusal for a release without checksums, got %v", err)
	}
	if _, _, ok := u.staged(); ok {
		t.Error("must not stage an unverifiable release")
	}
}

func TestCheckOnceSkipsDraftAndPrerelease(t *testing.T) {
	orig := appVersion
	appVersion = "1.0.0"
	defer func() { appVersion = orig }()

	for _, mode := range []string{"draft", "prerelease"} {
		mux := http.NewServeMux()
		srv := httptest.NewServer(mux)
		rel := ghRelease{TagName: "v9.9.9"}
		if mode == "draft" {
			rel.Draft = true
		} else {
			rel.Prerelease = true
		}
		mux.HandleFunc("/repos/"+updateRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(rel)
		})

		u := newUpdater(t.TempDir(), func(string) { t.Errorf("%s must not be staged", mode) })
		u.apiBase = srv.URL
		u.webBase = srv.URL
		if err := u.checkOnce(context.Background()); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
		srv.Close()
	}
}

func TestCheckOnceWritesPendingFlag(t *testing.T) {
	orig := appVersion
	appVersion = "1.0.0"
	defer func() { appVersion = orig }()

	payload := []byte("MZ\x90\x00 installer")
	digest := sha256.Sum256(payload)
	sum := hex.EncodeToString(digest[:])
	srv := newFakeRelease(t, "v1.3.0", payload, sum)

	u := newUpdater(t.TempDir(), nil)
	u.apiBase = srv.URL
	u.webBase = srv.URL
	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}

	p, ok := u.readPending()
	if !ok {
		t.Fatal("expected a pending flag on disk")
	}
	if p.Version != "1.3.0" {
		t.Errorf("flag version = %q, want 1.3.0", p.Version)
	}
	if p.SHA256 != sum {
		t.Errorf("flag digest = %q, want %q", p.SHA256, sum)
	}
	if p.Attempts != 0 {
		t.Errorf("fresh flag should have 0 attempts, got %d", p.Attempts)
	}
	// The flag must survive pruning, or the next launch forgets the update.
	u.pruneStaging(p.File)
	if _, ok := u.readPending(); !ok {
		t.Error("pruning removed the pending flag")
	}
}

// stageFakePending writes a flag and a matching installer, standing in for a
// previous session that finished its download.
func stageFakePending(t *testing.T, u *updater, version string) string {
	t.Helper()
	if err := os.MkdirAll(u.stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "GLM-Proxy-Setup-" + version + ".exe"
	path := filepath.Join(u.stagingDir, name)
	if err := os.WriteFile(path, []byte("MZ\x90\x00 staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := u.writePending(&pendingUpdate{Version: version, File: name}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPendingInstallerAcceptsNewerOnly(t *testing.T) {
	orig := appVersion
	defer func() { appVersion = orig }()

	u := newUpdater(t.TempDir(), nil)
	stageFakePending(t, u, "1.5.0")

	appVersion = "1.0.0"
	if _, path, ok := u.pendingInstaller(); !ok || path == "" {
		t.Error("a newer flagged version should be offered for install")
	}

	appVersion = "1.5.0"
	if _, _, ok := u.pendingInstaller(); ok {
		t.Error("a flag matching the running version must not reinstall")
	}

	appVersion = "2.0.0"
	if _, _, ok := u.pendingInstaller(); ok {
		t.Error("a flag older than the running version must not reinstall")
	}
}

func TestPendingInstallerRejectsMissingFile(t *testing.T) {
	orig := appVersion
	appVersion = "1.0.0"
	defer func() { appVersion = orig }()

	u := newUpdater(t.TempDir(), nil)
	path := stageFakePending(t, u, "1.5.0")
	os.Remove(path)

	if _, _, ok := u.pendingInstaller(); ok {
		t.Error("a flag whose installer vanished must not be applied")
	}
}

func TestApplyStartupUpdateClearsFlagAfterSuccessfulUpdate(t *testing.T) {
	orig := appVersion
	appVersion = "1.5.0"
	defer func() { appVersion = orig }()

	u := newUpdater(t.TempDir(), nil)
	stageFakePending(t, u, "1.5.0")

	// Already running the flagged version means the update landed.
	if u.applyStartupUpdate(context.Background()) {
		t.Error("must not reinstall the version already running")
	}
	if _, ok := u.readPending(); ok {
		t.Error("the satisfied flag should have been cleared")
	}
}

func TestApplyStartupUpdateAbandonsAfterRepeatedFailures(t *testing.T) {
	orig := appVersion
	appVersion = "1.0.0"
	defer func() { appVersion = orig }()

	u := newUpdater(t.TempDir(), nil)
	name := "GLM-Proxy-Setup-9.9.9.exe"
	os.MkdirAll(u.stagingDir, 0o755)
	os.WriteFile(filepath.Join(u.stagingDir, name), []byte("MZ\x90\x00"), 0o644)
	if err := u.writePending(&pendingUpdate{
		Version:  "9.9.9",
		File:     name,
		Attempts: updateMaxAttempts,
	}); err != nil {
		t.Fatal(err)
	}

	if u.applyStartupUpdate(context.Background()) {
		t.Error("an update that already burned its attempts must be abandoned")
	}
	if _, ok := u.readPending(); ok {
		t.Error("the abandoned flag should have been cleared")
	}
}

func TestUpdateAPIBaseRejectsNonLoopback(t *testing.T) {
	t.Setenv("GLM_UPDATE_API", "https://evil.example.com")
	if got := updateAPIBase(); got != defaultUpdateAPIBase {
		t.Errorf("a remote override must be ignored, got %q", got)
	}

	t.Setenv("GLM_UPDATE_API", "http://127.0.0.1:8099/")
	if got := updateAPIBase(); got != "http://127.0.0.1:8099" {
		t.Errorf("loopback override = %q", got)
	}

	t.Setenv("GLM_UPDATE_API", "http://localhost:8099")
	if got := updateAPIBase(); got != "http://localhost:8099" {
		t.Errorf("localhost override = %q", got)
	}

	t.Setenv("GLM_UPDATE_API", "")
	if got := updateAPIBase(); got != defaultUpdateAPIBase {
		t.Errorf("empty override should fall back, got %q", got)
	}
}

func TestUpdateIntervalOverride(t *testing.T) {
	t.Setenv("GLM_UPDATE_INTERVAL", "1m")
	if got := updateInterval(); got != time.Minute {
		t.Errorf("interval = %v, want 1m", got)
	}
	if got := updateFirstDelay(); got != 5*time.Second {
		t.Errorf("short interval should shorten the first delay, got %v", got)
	}

	t.Setenv("GLM_UPDATE_INTERVAL", "1ms")
	if got := updateInterval(); got != updateCheckInterval {
		t.Errorf("an absurdly short interval must be ignored, got %v", got)
	}

	t.Setenv("GLM_UPDATE_INTERVAL", "")
	if got := updateInterval(); got != updateCheckInterval {
		t.Errorf("default interval = %v", got)
	}
	if got := updateFirstDelay(); got != updateStartupDelay {
		t.Errorf("default first delay = %v", got)
	}
}

func TestAcquireSingleInstanceBlocksSecondHolder(t *testing.T) {
	name := "GLM-Proxy-Test-" + filepath.Base(t.TempDir())

	if !acquireSingleInstance(name) {
		t.Fatal("first acquisition should succeed")
	}
	first := instanceHandle
	if first == 0 {
		t.Fatal("expected a mutex handle to be retained")
	}
	defer func() {
		windows.CloseHandle(windows.Handle(first))
		instanceHandle = 0
	}()

	// A second attempt from this process must be refused. It retries for a few
	// seconds first, so allow for that before asserting.
	start := time.Now()
	if acquireSingleInstance(name) {
		t.Error("second acquisition should have been refused")
	}
	if waited := time.Since(start); waited < time.Second {
		t.Errorf("expected the retry window to be honoured, gave up after %v", waited)
	}
}

func TestAcquireSingleInstanceSucceedsAfterRelease(t *testing.T) {
	name := "GLM-Proxy-Test-Release-" + filepath.Base(t.TempDir())

	if !acquireSingleInstance(name) {
		t.Fatal("first acquisition should succeed")
	}
	h := instanceHandle
	windows.CloseHandle(windows.Handle(h))
	instanceHandle = 0

	// Once the holder lets go, a fresh instance must start. This is the
	// update-relaunch path: installer kills the old tray, then runs the new one.
	if !acquireSingleInstance(name) {
		t.Error("acquisition after release should succeed")
	}
	windows.CloseHandle(windows.Handle(instanceHandle))
	instanceHandle = 0
}

func TestReleaseIsNewer(t *testing.T) {
	orig := appVersion
	appVersion = "1.0.0"
	defer func() { appVersion = orig }()

	cases := []struct {
		rel  ghRelease
		want bool
		why  string
	}{
		{ghRelease{TagName: "v1.1.0"}, true, "newer release"},
		{ghRelease{TagName: "v1.0.0"}, false, "same version"},
		{ghRelease{TagName: "v0.9.0"}, false, "older version"},
		{ghRelease{TagName: "v2.0.0", Draft: true}, false, "draft"},
		{ghRelease{TagName: "v2.0.0", Prerelease: true}, false, "prerelease"},
		{ghRelease{TagName: "not-a-version"}, false, "unparsable tag"},
	}
	for _, c := range cases {
		if got := releaseIsNewer(&c.rel); got != c.want {
			t.Errorf("%s: releaseIsNewer(%q) = %v, want %v", c.why, c.rel.TagName, got, c.want)
		}
	}
	if releaseIsNewer(nil) {
		t.Error("nil release must not be considered newer")
	}
}

// TestStageReleaseHonoursDeadline proves the deadline is a real cap: a download
// that cannot finish is abandoned, stages nothing and leaves no partial file, so
// the caller is free to start the proxy.
func TestStageReleaseHonoursDeadline(t *testing.T) {
	orig := appVersion
	appVersion = "1.0.0"
	defer func() { appVersion = orig }()

	payload := []byte("MZ\x90\x00" + repeat("x", 4096))
	digest := sha256.Sum256(payload)
	sum := hex.EncodeToString(digest[:])

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/sums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", sum, updateAssetName)
	})
	// Dribble the body out far slower than the deadline permits.
	mux.HandleFunc("/setup.exe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		for i := 0; i < len(payload); i += 64 {
			end := i + 64
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := w.Write(payload[i:end]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	})

	rel := &ghRelease{
		TagName: "v1.2.0",
		Assets: []ghAsset{
			{Name: updateAssetName, URL: srv.URL + "/setup.exe", Size: int64(len(payload))},
			{Name: updateChecksumName, URL: srv.URL + "/sums.txt"},
		},
	}

	u := newUpdater(t.TempDir(), func(string) { t.Error("nothing should be staged past the deadline") })

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := u.stageRelease(ctx, rel)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the download to be cut off by the deadline")
	}
	if elapsed > 8*time.Second {
		t.Errorf("deadline was not respected: took %v", elapsed)
	}
	if _, _, ok := u.staged(); ok {
		t.Error("an aborted download must not be staged")
	}
	if _, ok := u.readPending(); ok {
		t.Error("an aborted download must not be flagged")
	}
	entries, _ := os.ReadDir(u.stagingDir)
	for _, e := range entries {
		t.Errorf("aborted download left %s behind", e.Name())
	}
}

// TestCheckWindowBoundsOnlyTheDecision guards the launch policy: deciding is
// quick, installing is not rushed.
func TestCheckWindowBoundsOnlyTheDecision(t *testing.T) {
	if updateCheckWindow > 10*time.Second {
		t.Errorf("check window %v is too long to sit in front of every launch", updateCheckWindow)
	}
	if updateApplyBudget <= updateCheckWindow {
		t.Errorf("apply budget %v must be far larger than the check window %v",
			updateApplyBudget, updateCheckWindow)
	}
}

// TestFetchLatestGivesUpOnASilentServer proves a launch is not held hostage by a
// server that accepts the connection then never answers.
func TestFetchLatestGivesUpOnASilentServer(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/repos/"+updateRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	u := newUpdater(t.TempDir(), nil)
	u.apiBase = srv.URL
	u.webBase = srv.URL

	start := time.Now()
	_, err := u.fetchLatest(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a server that never answers must produce an error")
	}
	if elapsed > updateCheckWindow+4*time.Second {
		t.Errorf("gave up after %v, expected close to %v", elapsed, updateCheckWindow)
	}
}
