// cmd/token-collector/main.go
// Standalone binary that seeds tokens.sqlite with device tokens harvested from
// chat.z.ai using a stealthed headless browser.
//
// Build:
//
//	go build -trimpath -ldflags="-s -w" -o token-collector ./cmd/token-collector
//
// Usage:
//
//	token-collector                        interactive prompts
//	token-collector --no-tui --batch 2     unattended, for the proxy's monitor
//	token-collector --tokens 750 --batch 3
//	token-collector --unsafe               raises the token and batch ceilings
//	token-collector --headed               visible browser, for debugging

package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mxschmitt/playwright-go"
	_ "modernc.org/sqlite" // pure-Go SQLite, no CGO needed
)

const (
	MaxTokens                = 1500
	UnsafeMaxTokens          = 1500
	DefaultTokens            = 850
	DefaultBatch             = 5
	MaxBatch                 = 9
	UnsafeMaxBatch           = 25
	SendWaitMs               = 10000
	MaxRetries               = 3
	TokenCollectionTimeoutMs = 90000
	URL                      = "https://chat.z.ai"

	// The model selector button's id encodes the selected model (e.g.
	// model-selector-x-preview-l-button, model-selector-glm-4_7-button),
	// so it is matched by prefix instead of a fixed id.
	ModelSelectorButton = `button[id^="model-selector-"][id$="-button"]`

	// Workers are parallel pages on a single browser, not parallel browsers.
	MaxParallel       = 3
	UnsafeMaxParallel = 5

	// Keep the stealth identity coherent with itself and with the region of
	// your egress IP. Proxying through Tokyo means Asia/Tokyo, ja-JP and a
	// matching Accept-Language.
	StealthTimezone = "America/New_York"
	StealthLocale   = "en-US"
)

var (
	unsafeFlag        = flag.Bool("unsafe", false, "increase token limit to 1500 and batch limit to 25")
	tokensFlag        = flag.Int("tokens", 0, "tokens per batch (0 = prompt)")
	batchFlag         = flag.Int("batch", 0, "number of batches (0 = prompt)")
	headedFlag        = flag.Bool("headed", false, "show browser window for debugging")
	parallelFlag      = flag.Int("parallel", 0, "parallel workers (pages) on a single browser; 0 = prompt y/N")
	blockTrackersFlag = flag.Bool("block-trackers", false, "enable URL allowlist filter to block trackers (off by default)")
	noTUIFlag         = flag.Bool("no-tui", false, "disable TUI, use plain text output")
	dbPathFlag        = flag.String("db-path", "tokens.sqlite", "path to the SQLite token database")
	freshFlag         = flag.Bool("fresh", false, "delete the existing database first instead of appending")
)

// The default 100% lets the heap double before collecting; 200% lets it grow
// threefold, which roughly halves GC pauses in this allocation-heavy workload.
func init() {
	debug.SetGCPercent(200)
}

// ============================================================================
// TUI
// ============================================================================

var spinnerChars = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// logCapture stores log lines in a ring buffer for the TUI to display.
type logCapture struct {
	mu     sync.Mutex
	lines  []string
	maxLen int
}

func (lc *logCapture) addLine(line string) {
	lc.mu.Lock()
	lc.lines = append(lc.lines, line)
	if len(lc.lines) > lc.maxLen {
		lc.lines = lc.lines[len(lc.lines)-lc.maxLen:]
	}
	lc.mu.Unlock()
}

func (lc *logCapture) Lines() []string {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	out := make([]string, len(lc.lines))
	copy(out, lc.lines)
	return out
}

// Atomics, so the TUI goroutine reads without locking.
var (
	tuiLogCapture      = &logCapture{maxLen: 1000}
	tuiStatus          atomic.Value // string
	tuiBatchesDone     atomic.Int64
	tuiTokensCollected atomic.Int64
	tuiTotalBatches    atomic.Int64
	tuiTokensPerBatch  atomic.Int64
	tuiWorkers         atomic.Int64
	tuiParallel        atomic.Bool
	tuiStartTime       atomic.Value // time.Time
	tuiDone            atomic.Bool
	tuiErr             atomic.Value // error
)

func init() {
	tuiStatus.Store("Initializing...")
	tuiStartTime.Store(time.Now())
}

func tuiSetStatus(s string) { tuiStatus.Store(s) }

type tickMsg time.Time
type doneMsg struct{ err error }

func tuiTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type tuiModel struct {
	width     int
	height    int
	logOffset int // lines scrolled up from bottom
	done      bool
	err       error
}

func (m tuiModel) Init() tea.Cmd { return tuiTick() }

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			m.logOffset++
		case "down", "j":
			if m.logOffset > 0 {
				m.logOffset--
			}
		case "g":
			m.logOffset = len(tuiLogCapture.Lines())
		case "G":
			m.logOffset = 0
		}
	case tickMsg:
		if tuiDone.Load() {
			m.done = true
			if v := tuiErr.Load(); v != nil {
				m.err = v.(error)
			}
			return m, tea.Quit
		}
		return m, tuiTick()
	case doneMsg:
		m.done = true
		m.err = msg.err
		return m, tea.Quit
	}
	return m, nil
}

func (m tuiModel) View() string {
	if m.height < 10 || m.width < 40 {
		return "Terminal too small (min 40x10). Resize or press q to quit."
	}

	// Color styles
	stTitle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	stLabel := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	stAcc := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	stWarn := lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	stDim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	stBar := lipgloss.NewStyle().Foreground(lipgloss.Color("99"))

	// Gather state from atomics (lock-free)
	status := "Initializing..."
	if v := tuiStatus.Load(); v != nil {
		status = v.(string)
	}
	bd := tuiBatchesDone.Load()
	tb := tuiTotalBatches.Load()
	tc := tuiTokensCollected.Load()
	tpb := tuiTokensPerBatch.Load()
	wk := tuiWorkers.Load()

	var st time.Time
	if v := tuiStartTime.Load(); v != nil {
		st = v.(time.Time)
	}
	elapsed := time.Since(st).Round(time.Second)

	// Header / status block
	var hdr strings.Builder
	hdr.WriteString(stTitle.Render("🔑 Token Collector"))
	hdr.WriteByte('\n')

	if m.done {
		if m.err != nil {
			hdr.WriteString(stLabel.Render("Status: ") + stWarn.Render(fmt.Sprintf("ERROR: %v", m.err)))
		} else {
			hdr.WriteString(stLabel.Render("Status: ") + stAcc.Render("✅ COMPLETE"))
		}
	} else {
		sp := spinnerChars[int(time.Now().UnixMilli()/200)%len(spinnerChars)]
		hdr.WriteString(stLabel.Render("Status: ") + fmt.Sprintf("%s %s", sp, stAcc.Render(status)))
	}
	hdr.WriteByte('\n')

	// Progress bar
	if tb > 0 {
		pct := float64(bd) / float64(tb)
		bw := 20
		f := int(pct * float64(bw))
		if f > bw {
			f = bw
		}
		bar := stBar.Render(strings.Repeat("█", f)) + stDim.Render(strings.Repeat("░", bw-f))
		hdr.WriteString(fmt.Sprintf("%s %s  %s\n",
			stLabel.Render("Progress:"),
			bar,
			stAcc.Render(fmt.Sprintf("%d/%d (%.0f%%)", bd, tb, pct*100))))
	}

	// Stats line
	target := tb * tpb
	stats := fmt.Sprintf("%s %s / %s",
		stLabel.Render("Tokens:"),
		stAcc.Render(fmt.Sprintf("%d", tc)),
		stDim.Render(fmt.Sprintf("%d", target)))
	if wk > 1 {
		stats += fmt.Sprintf("  %s %s", stLabel.Render("Workers:"), stAcc.Render(fmt.Sprintf("%d", wk)))
	}
	stats += fmt.Sprintf("  %s %s", stLabel.Render("Elapsed:"), stAcc.Render(elapsed.String()))
	hdr.WriteString(stats)
	hdr.WriteByte('\n')

	headerStr := hdr.String()
	headerLines := strings.Count(headerStr, "\n")

	// Log pane
	logH := m.height - headerLines - 4 // -4: sep + log header + sep + footer
	if logH < 2 {
		logH = 2
	}

	logs := tuiLogCapture.Lines()
	start := len(logs) - logH - m.logOffset
	if start < 0 {
		start = 0
	}
	end := start + logH
	if end > len(logs) {
		end = len(logs)
	}

	truncSt := lipgloss.NewStyle().MaxWidth(m.width)
	var logLines []string
	if len(logs) == 0 {
		logLines = []string{stDim.Render("(waiting for output...)")}
	} else if start < end {
		for _, l := range logs[start:end] {
			logLines = append(logLines, truncSt.Render(l))
		}
	} else {
		logLines = []string{stDim.Render("(no more logs)")}
	}
	logStr := strings.Join(logLines, "\n")

	// Separator and footer
	sep := stDim.Render(strings.Repeat("─", m.width))
	footer := stDim.Render(" ↑/↓ scroll  •  q quit")

	return headerStr + sep + "\n" + stLabel.Render("📋 Logs") + "\n" + logStr + "\n" + sep + "\n" + footer
}

func sleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func promptInt(reader *bufio.Reader, prompt string, def, max int) int {
	fmt.Print(prompt)
	line, err := reader.ReadString('\n')
	if err != nil {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	n, err := strconv.Atoi(line)
	if err != nil || n <= 0 {
		fmt.Printf("⚠️  Invalid input, using default %d.\n", def)
		return def
	}
	if n > max {
		fmt.Printf("⚠️  Capping to max %d.\n", max)
		return max
	}
	return n
}

func promptBool(reader *bufio.Reader, prompt string, def bool) bool {
	fmt.Print(prompt)
	line, err := reader.ReadString('\n')
	if err != nil {
		return def
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}

// tokenStore keeps one tuned SQLite connection open for the whole run.
// Reopening per batch would force a full fsync and a schema reparse each time.
type tokenStore struct {
	db          *sql.DB
	stmt        *sql.Stmt
	batchOffset int        // highest batch already in the database
	mu          sync.Mutex // serialise writes (SQLite is single-writer)
}

func openTokenStore(dbPath string) (*tokenStore, error) {
	dsn := "file:" + filepath.ToSlash(dbPath) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=cache_size(-65536)" + // 64 MB page cache
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=mmap_size(268435456)" + // 256 MB mmap
		"&_pragma=wal_autocheckpoint(1000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite serialises writes internally — one connection avoids
	// "database is locked" errors and avoids pool overhead.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Force the connection open so PRAGMAs take effect immediately.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tokens (
        id    INTEGER PRIMARY KEY AUTOINCREMENT,
        token TEXT    NOT NULL,
        batch INTEGER NOT NULL
    )`); err != nil {
		db.Close()
		return nil, err
	}
	// Index for fast batch lookups
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tokens_batch ON tokens(batch)`); err != nil {
		db.Close()
		return nil, err
	}

	stmt, err := db.Prepare(`INSERT INTO tokens (token, batch) VALUES (?, ?)`)
	if err != nil {
		db.Close()
		return nil, err
	}

	// Continue numbering past whatever is already stored, so the batch column
	// stays meaningful when appending to an existing database.
	var maxBatch int
	_ = db.QueryRow(`SELECT COALESCE(MAX(batch), 0) FROM tokens`).Scan(&maxBatch)

	return &tokenStore{db: db, stmt: stmt, batchOffset: maxBatch}, nil
}

func (ts *tokenStore) merge(batchNum int, tokens []string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	tx, err := ts.db.Begin()
	if err != nil {
		return err
	}
	// Rollback is a no-op after Commit, so this is safe.
	defer tx.Rollback()

	// Bind the prepared statement to this transaction.
	txStmt := tx.Stmt(ts.stmt)
	defer txStmt.Close()

	for _, tok := range tokens {
		if _, err := txStmt.Exec(tok, ts.batchOffset+batchNum); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (ts *tokenStore) close() {
	ts.stmt.Close()
	ts.db.Close()
}

// =====================================================================
// STEALTH LAYER — make headless Chromium indistinguishable from a real
// interactive Chrome running on a desktop.
// =====================================================================

// stealthChromeMajor resolves the bundled Chromium's major version once
// so the spoofed UA / userAgentData match the REAL engine version
// (a Chrome/131 UA on a Chromium/140 engine is itself a mismatch flag).
var (
	stealthVersionOnce sync.Once
	stealthMajor       string
)

func stealthChromeMajor(browser playwright.Browser) string {
	stealthVersionOnce.Do(func() {
		v := browser.Version()
		if i := strings.IndexByte(v, '.'); i > 0 {
			stealthMajor = v[:i]
		}
		if stealthMajor == "" {
			stealthMajor = "131"
		}
		fmt.Printf("🥷 Stealth identity: Chrome/%s on Windows 10 (x64)\n", stealthMajor)
	})
	return stealthMajor
}

func stealthUserAgent(major string) string {
	// Identical to a real headed Chrome on Windows 10 — no "HeadlessChrome".
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + major + ".0.0.0 Safari/537.36"
}

// stealthJSTemplate is injected via AddInitScript — it runs BEFORE any page
// script (including captcha SDKs) on every document and every frame.
// __VER__ is replaced with the real Chromium major version at runtime.
//
// Coverage:
//  1. navigator.webdriver        → false (prototype-level)
//  2. languages / language       → en-US
//  3. platform                   → Win32 (coherent with the UA)
//  4. hardwareConcurrency / deviceMemory
//  5. plugins / mimeTypes        → exact replica of real Chrome's
//     5 built-in PDF plugins
//  6. window.chrome              → runtime/app/loadTimes/csi objects
//  7. permissions.query          → notifications report 'default'
//  8. WebGL vendor/renderer      → ANGLE/Intel D3D11, not SwiftShader
//  9. userAgentData              → real Chrome brands, no HeadlessChrome
//  10. window/screen geometry     → coherent 2560×1440 display
//  11. iframe contentWindow       → chrome object present cross-frame
//  12. Notification.permission
//  13. Function.prototype.toString → every override above reports
//     "[native code]" (anti-tamper mask)
const stealthJSTemplate = `(function () {
    'use strict';

    var patched = [];
    function track(fn) { patched.push(fn); return fn; }

    var ua = navigator.userAgent || '';
    var isWin = ua.indexOf('Windows NT') !== -1;
    var isMac = ua.indexOf('Macintosh') !== -1;
    var platOS = isWin ? 'Windows' : (isMac ? 'macOS' : 'Linux');
    var platNav = isWin ? 'Win32' : (isMac ? 'MacIntel' : 'Linux x86_64');

    // 1) navigator.webdriver → false (matches normal, non-automated Chrome)
    try {
        Object.defineProperty(Navigator.prototype, 'webdriver', {
            get: track(function webdriver() { return false; }),
            configurable: true,
        });
    } catch (e) {}

    // 2) language / languages
    try {
        Object.defineProperty(Navigator.prototype, 'languages', {
            get: track(function languages() { return ['en-US', 'en']; }),
            configurable: true,
        });
        Object.defineProperty(Navigator.prototype, 'language', {
            get: track(function language() { return 'en-US'; }),
            configurable: true,
        });
    } catch (e) {}

    // 3) platform — must agree with the UA's OS token
    try {
        Object.defineProperty(Navigator.prototype, 'platform', {
            get: track(function platform() { return platNav; }),
            configurable: true,
        });
    } catch (e) {}

    // 4) CPU / memory
    try {
        Object.defineProperty(Navigator.prototype, 'hardwareConcurrency', {
            get: track(function hardwareConcurrency() { return 8; }),
            configurable: true,
        });
        if (!('deviceMemory' in navigator)) {
            Object.defineProperty(Navigator.prototype, 'deviceMemory', {
                get: track(function deviceMemory() { return 8; }),
                configurable: true,
            });
        }
    } catch (e) {}

    // 5) plugins / mimeTypes — replicate a real Chrome install exactly
    try {
        var names = ['PDF Viewer', 'Chrome PDF Viewer', 'Chromium PDF Viewer',
                     'Microsoft Edge PDF Viewer', 'WebKit built-in PDF'];
        var file = 'internal-pdf-viewer';
        var desc = 'Portable Document Format';
        var mimeDefs = [
            { type: 'application/pdf', suffixes: 'pdf' },
            { type: 'text/pdf', suffixes: 'pdf' },
        ];
        var plugins = [];
        names.forEach(function (n) {
            var plugin = Object.create(Plugin.prototype);
            var mimes = mimeDefs.map(function (m) {
                var mt = Object.create(MimeType.prototype);
                Object.defineProperties(mt, {
                    type: { value: m.type, enumerable: true },
                    suffixes: { value: m.suffixes, enumerable: true },
                    description: { value: desc, enumerable: true },
                    enabledPlugin: { value: plugin, enumerable: true },
                });
                return mt;
            });
            Object.defineProperties(plugin, {
                name: { value: n, enumerable: true },
                filename: { value: file, enumerable: true },
                description: { value: desc, enumerable: true },
                length: { value: mimes.length, enumerable: true },
            });
            mimes.forEach(function (m, i) {
                Object.defineProperty(plugin, String(i), { value: m, enumerable: true });
            });
            plugin.item = track(function item(i) { return mimes[i] || null; });
            plugin.namedItem = track(function namedItem(nm) {
                for (var i = 0; i < mimes.length; i++) { if (mimes[i].type === nm) return mimes[i]; }
                return null;
            });
            plugins.push(plugin);
        });
        var pa = Object.create(PluginArray.prototype);
        Object.defineProperty(pa, 'length', { value: plugins.length, enumerable: true });
        plugins.forEach(function (p, i) {
            Object.defineProperty(pa, String(i), { value: p, enumerable: true });
        });
        pa.item = track(function item(i) { return plugins[i] || null; });
        pa.namedItem = track(function namedItem(nm) {
            for (var i = 0; i < plugins.length; i++) { if (plugins[i].name === nm) return plugins[i]; }
            return null;
        });
        pa.refresh = track(function refresh() {});
        Object.defineProperty(Navigator.prototype, 'plugins', {
            get: track(function plugins() { return pa; }),
            configurable: true,
        });

        var navMimes = [plugins[0][0], plugins[0][1]];
        var ma = Object.create(MimeTypeArray.prototype);
        Object.defineProperty(ma, 'length', { value: navMimes.length, enumerable: true });
        navMimes.forEach(function (m, i) {
            Object.defineProperty(ma, String(i), { value: m, enumerable: true });
        });
        ma.item = track(function item(i) { return navMimes[i] || null; });
        ma.namedItem = track(function namedItem(nm) {
            for (var i = 0; i < navMimes.length; i++) { if (navMimes[i].type === nm) return navMimes[i]; }
            return null;
        });
        Object.defineProperty(Navigator.prototype, 'mimeTypes', {
            get: track(function mimeTypes() { return ma; }),
            configurable: true,
        });
    } catch (e) {}

    // 6) window.chrome — absent in headless, present in every real Chrome
    try {
        if (!window.chrome) { window.chrome = {}; }
        if (!window.chrome.runtime) {
            window.chrome.runtime = {
                PlatformOs: { MAC: 'mac', WIN: 'win', ANDROID: 'android', CROS: 'cros', LINUX: 'linux', OPENBSD: 'openbsd' },
                PlatformArch: { ARM: 'arm', X86_32: 'x86-32', X86_64: 'x86-64', MIPS: 'mips', MIPS64: 'mips64' },
                connect: track(function connect() { return undefined; }),
                sendMessage: track(function sendMessage() { return undefined; }),
            };
        }
        if (!window.chrome.app) {
            window.chrome.app = {
                isInstalled: false,
                InstallState: { DISABLED: 'disabled', INSTALLED: 'installed', NOT_INSTALLED: 'not_installed' },
                RunningState: { CANNOT_RUN: 'cannot_run', READY_TO_RUN: 'ready_to_run', RUNNING: 'running' },
                getDetails: track(function getDetails() { return null; }),
                getIsInstalled: track(function getIsInstalled() { return false; }),
                installState: track(function installState(cb) {
                    if (typeof cb === 'function') { cb('not_installed'); }
                }),
            };
        }
        if (!window.chrome.loadTimes) {
            window.chrome.loadTimes = track(function loadTimes() {
                var t = Date.now() / 1000;
                return {
                    requestTime: t, startLoadTime: t, commitLoadTime: t,
                    finishDocumentLoadTime: t, finishLoadTime: t, firstPaintTime: t,
                    firstPaintAfterLoadTime: 0, navigationType: 'Other',
                    wasFetchedViaSpdy: false, wasNpnNegotiated: true,
                    npnNegotiatedProtocol: 'h2', wasAlternateProtocolAvailable: false,
                    connectionInfo: 'h2',
                };
            });
        }
        if (!window.chrome.csi) {
            window.chrome.csi = track(function csi() {
                return { startE: Date.now(), onloadT: Date.now(), pageT: 10, tran: 15 };
            });
        }
    } catch (e) {}

    // 7) permissions.query — headless reports 'denied' for notifications
    try {
        if (window.navigator.permissions && window.navigator.permissions.query) {
            var oq = window.navigator.permissions.query.bind(window.navigator.permissions);
            window.navigator.permissions.query = track(function query(p) {
                if (p && p.name === 'notifications') {
                    return Promise.resolve({ state: 'default', onchange: null });
                }
                return oq(p);
            });
        }
    } catch (e) {}

    // 8) WebGL — headless leaks SwiftShader; fake a plausible GPU stack
    try {
        var vendor = isWin ? 'Google Inc. (Intel)' : (isMac ? 'Google Inc. (Apple)' : 'Google Inc. (Intel)');
        var renderer = isWin
            ? 'ANGLE (Intel, Intel(R) UHD Graphics 630 (D3D11), D3D11)'
            : (isMac
                ? 'ANGLE (Apple, ANGLE Metal Renderer: Apple M1, Unspecified Version)'
                : 'ANGLE (Intel, Mesa Intel(R) UHD Graphics 630 (CFL GT2), OpenGL 4.6)');
        function patchGet(proto) {
            var orig = proto.getParameter;
            proto.getParameter = track(function getParameter(p) {
                if (p === 37445) { return vendor; }
                if (p === 37446) { return renderer; }
                return orig.apply(this, arguments);
            });
        }
        if (typeof WebGLRenderingContext !== 'undefined') { patchGet(WebGLRenderingContext.prototype); }
        if (typeof WebGL2RenderingContext !== 'undefined') { patchGet(WebGL2RenderingContext.prototype); }
    } catch (e) {}

    // 9) userAgentData — headless leaks a 'HeadlessChrome' brand
    try {
        if (navigator.userAgentData) {
            var uad = {
                brands: [
                    { brand: 'Chromium', version: '__VER__' },
                    { brand: 'Google Chrome', version: '__VER__' },
                    { brand: 'Not_A Brand', version: '24' },
                ],
                mobile: false,
                platform: platOS,
                getHighEntropyValues: track(function getHighEntropyValues(hints) {
                    return Promise.resolve({
                        architecture: 'x86',
                        bitness: '64',
                        model: '',
                        platform: platOS,
                        platformVersion: isWin ? '15.0.0' : (isMac ? '14.1.0' : '6.5.0'),
                        uaFullVersion: '__VER__.0.0.0',
                        fullVersionList: [
                            { brand: 'Chromium', version: '__VER__.0.0.0' },
                            { brand: 'Google Chrome', version: '__VER__.0.0.0' },
                            { brand: 'Not_A Brand', version: '24.0.0.0' },
                        ],
                    });
                }),
            };
            Object.defineProperty(Navigator.prototype, 'userAgentData', {
                get: track(function userAgentData() { return uad; }),
                configurable: true,
            });
        }
    } catch (e) {}

    // 10) window / screen geometry — headless reports a 0-sized outer window
    try {
        Object.defineProperty(window, 'outerWidth', {
            get: track(function outerWidth() { return 1920; }), configurable: true,
        });
        Object.defineProperty(window, 'outerHeight', {
            get: track(function outerHeight() { return 1160; }), configurable: true,
        });
        Object.defineProperty(Screen.prototype, 'width', {
            get: track(function width() { return 2560; }), configurable: true,
        });
        Object.defineProperty(Screen.prototype, 'height', {
            get: track(function height() { return 1440; }), configurable: true,
        });
        Object.defineProperty(Screen.prototype, 'availWidth', {
            get: track(function availWidth() { return 2560; }), configurable: true,
        });
        Object.defineProperty(Screen.prototype, 'availHeight', {
            get: track(function availHeight() { return 1400; }), configurable: true,
        });
        Object.defineProperty(Screen.prototype, 'colorDepth', {
            get: track(function colorDepth() { return 24; }), configurable: true,
        });
        Object.defineProperty(Screen.prototype, 'pixelDepth', {
            get: track(function pixelDepth() { return 24; }), configurable: true,
        });
    } catch (e) {}

    // 11) iframes — captcha SDKs check contentWindow.chrome cross-frame
    try {
        var d = Object.getOwnPropertyDescriptor(HTMLIFrameElement.prototype, 'contentWindow');
        if (d && d.get) {
            var og = d.get;
            Object.defineProperty(HTMLIFrameElement.prototype, 'contentWindow', {
                get: track(function contentWindow() {
                    var w = og.call(this);
                    try {
                        if (w && !w.chrome && window.chrome) { w.chrome = window.chrome; }
                    } catch (e) {}
                    return w;
                }),
                configurable: true,
            });
        }
    } catch (e) {}

    // 12) Notification.permission
    try {
        if (typeof Notification !== 'undefined') {
            Object.defineProperty(Notification, 'permission', {
                get: track(function permission() { return 'default'; }),
                configurable: true,
            });
        }
    } catch (e) {}

    // 13) toString mask — every function patched above must report
    //     "[native code]" or the tampering itself becomes detectable.
    try {
        var origToString = Function.prototype.toString;
        var toStringProxy = new Proxy(origToString, {
            apply: function (target, thisArg, args) {
                if (thisArg && patched.indexOf(thisArg) !== -1) {
                    return 'function ' + (thisArg.name || '') + '() { [native code] }';
                }
                return Reflect.apply(target, thisArg, args);
            },
        });
        patched.push(toStringProxy);
        Function.prototype.toString = toStringProxy;
    } catch (e) {}
})();`

// =====================================================================
// HUMAN INPUT SYNTHESIS — anti-bot systems watch pointer trajectories,
// dwell times, and typing cadence. Synthesize them.
// =====================================================================

var humanRand = rand.New(rand.NewSource(time.Now().UnixNano()))

func jitter(min, max float64) float64 {
	return min + humanRand.Float64()*(max-min)
}

func humanPause(msMin, msMax int) {
	time.Sleep(time.Duration(jitter(float64(msMin), float64(msMax))) * time.Millisecond)
}

// humanMouseTo sweeps the cursor from a random point toward (tx,ty) with
// smoothstep easing and per-step jitter — mimics a human pointer path.
func humanMouseTo(page playwright.Page, tx, ty float64) error {
	x := jitter(300, 1500)
	y := jitter(200, 800)
	steps := 12 + humanRand.Intn(10)
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		e := t * t * (3 - 2*t) // smoothstep: slow → fast → slow
		nx := x + (tx-x)*e + jitter(-6, 6)
		ny := y + (ty-y)*e + jitter(-6, 6)
		if err := page.Mouse().Move(nx, ny); err != nil {
			return err
		}
		time.Sleep(time.Duration(jitter(8, 22)) * time.Millisecond)
	}
	// Final exact landing on the target.
	return page.Mouse().Move(tx, ty)
}

// humanClick moves the cursor along an eased path to a random point inside
// the element, dwells briefly (decision time), then presses and releases.
func humanClick(page playwright.Page, loc playwright.Locator) error {
	box, err := loc.BoundingBox()
	if err != nil {
		return err
	}
	if box == nil {
		return fmt.Errorf("element has no bounding box")
	}
	cx := box.X + box.Width*jitter(0.35, 0.65)
	cy := box.Y + box.Height*jitter(0.35, 0.65)
	if err := humanMouseTo(page, cx, cy); err != nil {
		return err
	}
	humanPause(60, 180) // dwell: "reading" the button before committing
	if err := page.Mouse().Down(); err != nil {
		return err
	}
	humanPause(45, 110) // human press-hold duration
	return page.Mouse().Up()
}

var preferredModelValues = []string{"glm-5.1", "glm-5", "glm-4.7"}

func selectWorkingModel(page playwright.Page) error {
	modelBtn := page.Locator(ModelSelectorButton).First()
	if id, _ := modelBtn.GetAttribute("id"); id != "" {
		for _, v := range preferredModelValues {
			if strings.Contains(id, strings.ReplaceAll(v, ".", "_")) {
				return nil
			}
		}
	}

	if err := humanClick(page, modelBtn); err != nil {
		return fmt.Errorf("open model menu: %w", err)
	}
	humanPause(150, 400)

	menu := page.Locator("[data-dropdown-menu-content][data-state='open']")
	if err := menu.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(8000)}); err != nil {
		return fmt.Errorf("model dropdown did not open: %w", err)
	}

	option := page.Locator("button[data-value]").First()
	for _, v := range preferredModelValues {
		cand := page.Locator(fmt.Sprintf("button[data-value=%q]", v))
		if err := cand.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(1500)}); err == nil {
			option = cand
			break
		}
	}
	if err := option.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(3000)}); err != nil {
		return fmt.Errorf("no model option available: %w", err)
	}

	_ = option.ScrollIntoViewIfNeeded(playwright.LocatorScrollIntoViewIfNeededOptions{Timeout: playwright.Float(3000)})
	humanPause(100, 250)
	if err := humanClick(page, option); err != nil {
		return fmt.Errorf("click model option: %w", err)
	}
	_ = menu.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateHidden,
		Timeout: playwright.Float(3000),
	})
	humanPause(200, 500)
	return nil
}

// collectTokensOnPage harvests up to total device tokens from one page. The page
// is reused across batches, with route handlers installed once in newWorkerPage,
// so each call force-reloads by re-navigating.
func collectTokensOnPage(page playwright.Page, total int) ([]string, error) {
	if _, err := page.Goto(URL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return nil, fmt.Errorf("goto: %w", err)
	}

	// Wait for both elements concurrently
	tuiSetStatus("Locating UI elements...")
	fmt.Println("  Locating UI elements in parallel...")
	var (
		err1, err2 error
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		err1 = page.Locator(ModelSelectorButton).First().WaitFor(
			playwright.LocatorWaitForOptions{Timeout: playwright.Float(15000)},
		)
	}()
	go func() {
		defer wg.Done()
		err2 = page.Locator("#chat-input").WaitFor(
			playwright.LocatorWaitForOptions{Timeout: playwright.Float(15000)},
		)
	}()
	wg.Wait()

	if err1 != nil {
		return nil, fmt.Errorf("model button not found: %w", err1)
	}
	if err2 != nil {
		return nil, fmt.Errorf("textarea not found: %w", err2)
	}
	fmt.Println("✅ Model button & textarea found")
	if err := selectWorkingModel(page); err != nil {
		fmt.Printf("⚠️  model switch skipped: %v — using the default model\n", err)
		_ = page.Keyboard().Press("Escape")
		humanPause(150, 400)
	} else {
		fmt.Println("✅ Model ready")
	}

	textarea := page.Locator("#chat-input")

	// Behavioral warm-up: nobody lands on a page and instantly acts
	tuiSetStatus("Simulating human interaction...")
	fmt.Println("🧍 Human warm-up: cursor drift + micro-scroll...")
	_ = humanMouseTo(page, jitter(500, 1300), jitter(250, 650))
	humanPause(300, 800)
	_ = page.Mouse().Wheel(0, jitter(120, 320))
	humanPause(180, 450)
	_ = page.Mouse().Wheel(0, -jitter(60, 180))
	humanPause(250, 600)

	// Click into the textarea like a person, then type character by
	// character with irregular inter-key delays.
	if err := humanClick(page, textarea); err != nil {
		return nil, fmt.Errorf("human click on textarea: %w", err)
	}
	humanPause(120, 300)
	for _, r := range "__" {
		if err := page.Keyboard().Type(string(r)); err != nil {
			return nil, fmt.Errorf("type char: %w", err)
		}
		humanPause(50, 140)
	}
	fmt.Println(`✅ Typed "__" with human-like cadence`)

	sendBtn := page.Locator("#send-message-button")
	if err := sendBtn.WaitFor(
		playwright.LocatorWaitForOptions{Timeout: playwright.Float(5000)},
	); err != nil {
		return nil, fmt.Errorf("send button not found: %w", err)
	}
	humanPause(200, 500) // brief hesitation before firing
	if err := humanClick(page, sendBtn); err != nil {
		return nil, fmt.Errorf("human click on send: %w", err)
	}
	fmt.Println("✅ Send clicked (eased mouse path + button dwell)")

	fmt.Printf("⏳ Waiting %dms for token endpoint to initialize...\n", SendWaitMs)
	sleep(SendWaitMs)

	tuiSetStatus(fmt.Sprintf("Collecting %d tokens...", total))
	fmt.Println("🚀 Collecting tokens...")
	t0 := time.Now()

	type evalResult struct {
		val interface{}
		err error
	}
	resultCh := make(chan evalResult, 1)

	go func() {
		val, err := page.Evaluate(`async (args) => {
            const total = args.total;
            const out = new Array(total);
            for (let i = 0; i < total; i++) {
                const tok = window.z_um.getToken();
                out[i] = (tok && typeof tok.then === 'function') ? await tok : tok;
                if (i % 50 === 0) {
                    await new Promise(r => setTimeout(r, 0));
                }
            }
            return out;
        }`, map[string]interface{}{"total": total})
		resultCh <- evalResult{val, err}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			return nil, fmt.Errorf("evaluate: %w", res.err)
		}
		arr, ok := res.val.([]interface{})
		if !ok {
			return nil, fmt.Errorf("unexpected evaluate result type: %T", res.val)
		}
		// Pre-allocate with exact capacity — avoids slice growth reallocations.
		tokens := make([]string, 0, len(arr))
		for _, v := range arr {
			if s, ok := v.(string); ok {
				tokens = append(tokens, s)
			} else if v != nil {
				tokens = append(tokens, fmt.Sprintf("%v", v))
			}
		}
		elapsed := time.Since(t0).Seconds()
		fmt.Printf("✅ Collected %d tokens in %.2fs\n", len(tokens), elapsed)
		return tokens, nil

	case <-time.After(TokenCollectionTimeoutMs * time.Millisecond):
		return nil, fmt.Errorf("⏱️ token collection timed out after %ds", TokenCollectionTimeoutMs/1000)
	}
}

// newWorkerPage builds a worker with its own BrowserContext carrying a coherent
// fingerprint (UA, locale, timezone, viewport, screen) plus the stealth script
// injected into every frame before any page JavaScript runs. The optional route
// allowlist is installed once here rather than per batch.
func newWorkerPage(browser playwright.Browser) (playwright.BrowserContext, playwright.Page, error) {
	major := stealthChromeMajor(browser)
	ua := stealthUserAgent(major)
	stealthJS := strings.ReplaceAll(stealthJSTemplate, "__VER__", major)

	ctx, err := browser.NewContext(playwright.BrowserNewContextOptions{
		UserAgent:         playwright.String(ua),
		Locale:            playwright.String(StealthLocale),
		TimezoneId:        playwright.String(StealthTimezone),
		ColorScheme:       playwright.ColorSchemeLight,
		DeviceScaleFactor: playwright.Float(1),
		Viewport:          &playwright.Size{Width: 1920, Height: 1080},
		Screen:            &playwright.Size{Width: 1920, Height: 1440},
		ExtraHttpHeaders:  map[string]string{"Accept-Language": "en-US,en;q=0.9"},
	})
	if err != nil {
		return nil, nil, err
	}

	page, err := ctx.NewPage()
	if err != nil {
		_ = ctx.Close()
		return nil, nil, err
	}

	// Stealth init script — runs before any page JS on every frame,
	// including captcha iframes.
	if err := page.AddInitScript(playwright.Script{Content: playwright.String(stealthJS)}); err != nil {
		_ = ctx.Close()
		return nil, nil, fmt.Errorf("stealth init script: %w", err)
	}

	if *blockTrackersFlag {
		if err := page.Route("**/*", func(route playwright.Route) {
			if urlAllowed(route.Request().URL()) {
				route.Continue()
			} else {
				route.Abort()
			}
		}); err != nil {
			_ = ctx.Close()
			return nil, nil, fmt.Errorf("route setup: %w", err)
		}
	}
	return ctx, page, nil
}

// runBatch collects one batch, retrying up to MaxRetries. The page is reused
// across batches and reloaded on every attempt.
func runBatch(page playwright.Page, total, batchNum int) ([]string, error) {
	var lastErr error
	for attempt := 1; attempt <= MaxRetries; attempt++ {
		tuiSetStatus(fmt.Sprintf("Batch %d — attempt %d/%d", batchNum, attempt, MaxRetries))
		fmt.Printf("\n🔄 [Batch %d] Attempt %d of %d\n", batchNum, attempt, MaxRetries)

		tokens, err := collectTokensOnPage(page, total)

		if err != nil {
			lastErr = err
			fmt.Printf("❌ Attempt %d failed: %v\n", attempt, err)
			if attempt == MaxRetries {
				fmt.Fprintln(os.Stderr, "🚫 All retries exhausted.")
				break
			}
			fmt.Println("♻️  Retrying with a forced page reload...")
			continue
		}
		return tokens, nil
	}
	return nil, fmt.Errorf("batch %d failed: %w", batchNum, lastErr)
}

// runParallel spreads the batches across N pages on a single browser.
func runParallel(browser playwright.Browser, tokenCount, batchCount, workers int, ts *tokenStore, dbPath string) (int, error) {
	var (
		aborted  atomic.Bool
		totalCol atomic.Int64
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)

	batchCh := make(chan int, batchCount)
	for b := 1; b <= batchCount; b++ {
		batchCh <- b
	}
	close(batchCh)

	for w := 1; w <= workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			// Each worker keeps ONE stealth context + page open for all its
			// batches; every batch force-reloads the page instead of opening
			// a new one. Closing the context tears down the page with it.
			ctx, page, perr := newWorkerPage(browser)
			if perr != nil {
				once.Do(func() {
					firstErr = fmt.Errorf("worker %d page: %w", workerID, perr)
					aborted.Store(true)
				})
				return
			}
			defer ctx.Close()
			for batchNum := range batchCh {
				// Lock-free check — no mutex contention.
				if aborted.Load() {
					return
				}

				fmt.Printf("\n👷 [Worker %d] starting batch %d\n", workerID, batchNum)

				tokens, err := runBatch(page, tokenCount, batchNum)
				if err != nil {
					once.Do(func() {
						firstErr = err
						aborted.Store(true)
					})
					return
				}

				dbErr := ts.merge(batchNum, tokens)
				if dbErr != nil {
					once.Do(func() {
						firstErr = fmt.Errorf("database merge: %w", dbErr)
						aborted.Store(true)
					})
					return
				}

				// Lock-free atomic add — no mutex.
				cur := totalCol.Add(int64(len(tokens)))

				tuiBatchesDone.Add(1)
				tuiTokensCollected.Add(int64(len(tokens)))
				fmt.Printf("✅ [Worker %d] batch %d done — %d tokens (running total: %d)\n",
					workerID, batchNum, len(tokens), cur)
			}
		}(w)
	}
	wg.Wait()
	return int(totalCol.Load()), firstErr
}

// chromiumPerfArgs disables background throttling, unneeded services and
// automation detection, keeping the renderer hot and avoiding IPC storms.
var chromiumPerfArgs = []string{
	"--disable-blink-features=AutomationControlled",
	"--disable-background-timer-throttling",
	"--disable-renderer-backgrounding",
	"--disable-backgrounding-occluded-windows",
	"--disable-ipc-flooding-protection",
	"--disable-background-networking",
	"--disable-default-apps",
	"--disable-extensions",
	"--disable-sync",
	"--disable-translate",
	"--disable-component-update",
	"--disable-client-side-phishing-detection",
	"--disable-hang-monitor",
	"--disable-popup-blocking",
	"--disable-prompt-on-repost",
	"--disable-domain-reliability",
	"--disable-features=Translate,MediaRouter,OptimizationHints",
	"--no-first-run",
	"--no-default-browser-check",
	"--metrics-recording-only",
	"--safebrowsing-disable-auto-update",
	"--password-store=basic",
	"--use-mock-keychain",
	"--lang=en-US",
}

// launchBrowser tells Playwright the browser is headed, then passes
// --headless=new itself. That runs the new headless engine — real Blink, real
// GPU pipeline, real fingerprint surface, just no window — so it works on a bare
// server with no display while the init script patches the remaining headless
// tells (outerWidth=0, SwiftShader, missing chrome object, HeadlessChrome UA
// brand). --enable-automation is stripped from Playwright's defaults so the
// automation flag never reaches the engine.
func launchBrowser(pw *playwright.Playwright, headed bool) (playwright.Browser, error) {
	base := append([]string{}, chromiumPerfArgs...)

	if headed {
		// Real visible window for debugging — stealth script still applies.
		return pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
			Headless:          playwright.Bool(false),
			Args:              base,
			IgnoreDefaultArgs: []string{"--enable-automation"},
		})
	}

	args := append(base, "--headless=new", "--window-size=1920,1080")
	b, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless:          playwright.Bool(false), // OUR flag controls headlessness
		Args:              args,
		IgnoreDefaultArgs: []string{"--enable-automation"},
	})
	if err == nil {
		return b, nil
	}

	// Fallback for ancient Chromium builds that reject --headless=new:
	// classic headless — the stealth script still patches the surface.
	fmt.Fprintf(os.Stderr, "⚠️  --headless=new launch failed (%v); falling back to classic headless\n", err)
	return pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless:          playwright.Bool(true),
		Args:              base,
		IgnoreDefaultArgs: []string{"--enable-automation"},
	})
}

// Network allowlist. Only the wildcard rules need a regex; prefix and exact
// rules are matched with strings.HasPrefix and ==.
var (
	// https://z-cdn.chatglm.cn/z-ai/frontend/prod-fe-*/assets/index-*.js
	reZCDN = regexp.MustCompile(`^https://z-cdn\.chatglm\.cn/z-ai/frontend/prod-fe-[^/]+/assets/index-[^/]+\.js$`)
	// https://cloudauth-device-dualstack.*aliyuncs.com/
	reCloudAuth = regexp.MustCompile(`^https://cloudauth-device-dualstack\.[^/]*aliyuncs\.com/`)
	// https://g.alicdn.com/captcha-frontend/FeiLin/*/feilin*.*.js
	reFeiLin = regexp.MustCompile(`^https://g\.alicdn\.com/captcha-frontend/FeiLin/[^/]+/feilin[^/]*\.[^/]*\.js$`)
)

// urlAllowed checks a URL against the allowlist.
// Fast path: prefix checks via strings.HasPrefix (~5 ns each).
// Slow path: regex only for wildcard patterns (3 of 5 rules).
// Switch short-circuits on first match — most requests are decided
// in O(prefix_length) without ever touching the regex engine.
func urlAllowed(u string) bool {
	switch {
	// 1. Entire chat.z.ai domain — also allow wss:// for WebSocket upgrades
	case strings.HasPrefix(u, "https://chat.z.ai/"), strings.HasPrefix(u, "wss://chat.z.ai/"):
		return true
	// 2. z-cdn build assets (prefix filter → regex confirm)
	case strings.HasPrefix(u, "https://z-cdn.chatglm.cn/z-ai/frontend/prod-fe-"):
		return reZCDN.MatchString(u)
	// 3. Exact Aliyun captcha script (string equality, no regex)
	case u == "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js":
		return true
	// 4. cloudauth-device-dualstack.*aliyuncs.com (prefix filter → regex confirm)
	case strings.HasPrefix(u, "https://cloudauth-device-dualstack."):
		return reCloudAuth.MatchString(u)
	// 5. FeiLin captcha assets (prefix filter → regex confirm)
	case strings.HasPrefix(u, "https://g.alicdn.com/captcha-frontend/FeiLin/"):
		return reFeiLin.MatchString(u)
	}
	return false
}

func run(tokenCount, batchCount, parallelWorkers int, headed bool) error {
	// Install Playwright browsers (best-effort)
	tuiSetStatus("Installing Playwright...")
	fmt.Println("⏳ Ensuring Playwright Chromium browser is installed...")
	if err := playwright.Install(&playwright.RunOptions{
		Browsers: []string{"chromium"},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  playwright install: %v (continuing anyway)\n", err)
	}

	tuiSetStatus("Launching browser...")
	if !headed {
		fmt.Println("🥷 Stealth mode: new-headless engine + real-Chrome fingerprint emulation + human input synthesis")
	}
	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf("playwright run: %w", err)
	}
	defer pw.Stop()

	browser, err := launchBrowser(pw, headed)
	if err != nil {
		return fmt.Errorf("browser launch: %w", err)
	}
	defer browser.Close()

	// Appending by default matters for unattended replenishment: the proxy's
	// token monitor invokes this while the store still holds usable tokens, and
	// wiping would destroy them. --fresh restores the old reset behaviour.
	dbPath := *dbPathFlag
	if dbPath == "" {
		dbPath = "tokens.sqlite"
	}
	if abs, err := filepath.Abs(dbPath); err == nil {
		dbPath = abs
	}
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db directory: %w", err)
		}
	}
	if *freshFlag {
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	}

	// Open DB once and keep it — avoids per-batch open/close/fsync.
	ts, err := openTokenStore(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer ts.close()

	if parallelWorkers > 1 && batchCount > 1 {
		tuiSetStatus(fmt.Sprintf("Parallel: %d workers", parallelWorkers))
		fmt.Printf("\n🚀 PARALLEL mode: %d worker page(s) on a single browser\n", parallelWorkers)
		totalCollected, err := runParallel(browser, tokenCount, batchCount, parallelWorkers, ts, dbPath)
		if err != nil {
			return err
		}

		fmt.Printf("\n══════════════════════════════════════════\n")
		fmt.Printf("  ✅ ALL BATCHES COMPLETE (parallel: %d workers)\n", parallelWorkers)
		fmt.Printf("  📦 %d batches × %d tokens = %d total collected\n",
			batchCount, tokenCount, totalCollected)
		if info, err := os.Stat(dbPath); err == nil {
			fmt.Printf("  💾 %s (%.1f KB)\n", dbPath, float64(info.Size())/1024.0)
		}
		fmt.Printf("══════════════════════════════════════════\n")

		return nil
	}

	tuiSetStatus("Starting sequential batches...")
	// Keep ONE stealth context + page open across all batches; each batch
	// force-reloads the page instead of opening a new one.
	ctx, page, err := newWorkerPage(browser)
	if err != nil {
		return fmt.Errorf("page create: %w", err)
	}
	defer ctx.Close()

	totalCollected := 0
	for b := 1; b <= batchCount; b++ {
		fmt.Printf("\n══════════════════════════════════════════\n")
		fmt.Printf("  BATCH %d of %d\n", b, batchCount)
		fmt.Printf("══════════════════════════════════════════\n")

		tokens, err := runBatch(page, tokenCount, b)
		if err != nil {
			return err
		}

		if err := ts.merge(b, tokens); err != nil {
			return fmt.Errorf("database merge: %w", err)
		}

		totalCollected += len(tokens)
		tuiBatchesDone.Add(1)
		tuiTokensCollected.Store(int64(totalCollected))

		if info, err := os.Stat(dbPath); err == nil {
			fmt.Printf("💾 Database: %s (%.1f KB) — %d tokens total across %d batch(es)\n",
				dbPath, float64(info.Size())/1024.0, totalCollected, b)
		}
	}

	fmt.Printf("\n══════════════════════════════════════════\n")
	fmt.Printf("  ✅ ALL BATCHES COMPLETE\n")
	fmt.Printf("  📦 %d batches × %d tokens = %d total collected\n", batchCount, tokenCount, totalCollected)
	if info, err := os.Stat(dbPath); err == nil {
		fmt.Printf("  💾 %s (%.1f KB)\n", dbPath, float64(info.Size())/1024.0)
	}
	fmt.Printf("══════════════════════════════════════════\n")

	return nil
}

func main() {
	flag.Parse()

	// Apply --unsafe limits
	maxTokens := MaxTokens
	maxBatch := MaxBatch
	if *unsafeFlag {
		maxTokens = UnsafeMaxTokens
		maxBatch = UnsafeMaxBatch
		fmt.Println("⚠️  --unsafe mode enabled: token limit=1500, batch limit=25")
	}

	reader := bufio.NewReader(os.Stdin)
	unattended := *noTUIFlag

	tokenCount := *tokensFlag
	if tokenCount <= 0 {
		if unattended {
			tokenCount = DefaultTokens
		} else {
			tokenCount = promptInt(reader,
				fmt.Sprintf("How many tokens to collect per batch? [default: %d, max: %d] ", DefaultTokens, maxTokens),
				DefaultTokens, maxTokens)
		}
	} else if tokenCount > maxTokens {
		fmt.Printf("⚠️  Capping tokens to max %d.\n", maxTokens)
		tokenCount = maxTokens
	}

	batchCount := *batchFlag
	if batchCount <= 0 {
		if unattended {
			batchCount = DefaultBatch
		} else {
			batchCount = promptInt(reader,
				fmt.Sprintf("How many batches? [default: %d, max: %d] ", DefaultBatch, maxBatch),
				DefaultBatch, maxBatch)
		}
	} else if batchCount > maxBatch {
		fmt.Printf("⚠️  Capping batch to max %d.\n", maxBatch)
		batchCount = maxBatch
	}

	maxParallel := MaxParallel
	if *unsafeFlag {
		maxParallel = UnsafeMaxParallel
	}

	parallelWorkers := *parallelFlag
	if parallelWorkers == 0 {
		if !unattended && promptBool(reader, "Enable parallel workers (parallel pages on one browser)? [y/N] ", false) {
			parallelWorkers = promptInt(reader,
				fmt.Sprintf("How many parallel workers? [default: %d, max: %d] ", maxParallel, maxParallel),
				maxParallel, maxParallel)
		}
	} else if parallelWorkers < 0 {
		parallelWorkers = 0
	} else if parallelWorkers > maxParallel {
		fmt.Printf("⚠️  Capping parallel workers to max %d.\n", maxParallel)
		parallelWorkers = maxParallel
	}

	// Set up before the plan is printed, so the plan lands in the TUI log.
	useTUI := !*noTUIFlag
	var origStdout, origStderr *os.File
	var pipeWriter *os.File

	if useTUI {
		origStdout = os.Stdout
		origStderr = os.Stderr

		r, w, perr := os.Pipe()
		if perr != nil {
			fmt.Fprintf(os.Stderr, "pipe error: %v\n", perr)
			os.Exit(1)
		}
		pipeWriter = w
		os.Stdout = w
		os.Stderr = w

		// Goroutine: read piped output → logCapture ring buffer.
		// Non-blocking; uses 1MB scanner buffer for long lines.
		go func() {
			scanner := bufio.NewScanner(r)
			scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
			for scanner.Scan() {
				tuiLogCapture.addLine(scanner.Text())
			}
		}()

		tuiTotalBatches.Store(int64(batchCount))
		tuiTokensPerBatch.Store(int64(tokenCount))
		wk := parallelWorkers
		if wk < 1 {
			wk = 1
		}
		tuiWorkers.Store(int64(wk))
		tuiParallel.Store(parallelWorkers > 1)
		tuiStartTime.Store(time.Now())
		tuiStatus.Store("Starting...")
	}

	fmt.Printf("\n🎯 Plan: %d tokens × %d batches = %d total tokens",
		tokenCount, batchCount, tokenCount*batchCount)
	if parallelWorkers > 1 {
		fmt.Printf("  (parallel: %d workers)", parallelWorkers)
	}
	fmt.Println()

	if !useTUI {
		// Plain text mode — no TUI, original behaviour
		if err := run(tokenCount, batchCount, parallelWorkers, *headedFlag); err != nil {
			fmt.Fprintf(os.Stderr, "\n🚫 Fatal error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("\n🎉 Script finished successfully.")
		return
	}

	// tea.WithOutput(origStdout) sends TUI rendering to the real terminal
	// while fmt.Println goes to the pipe → logCapture.
	p := tea.NewProgram(tuiModel{},
		tea.WithAltScreen(),
		tea.WithOutput(origStdout),
	)

	go func() {
		err := run(tokenCount, batchCount, parallelWorkers, *headedFlag)
		if err == nil {
			tuiSetStatus("Complete!")
		}
		tuiDone.Store(true)
		if err != nil {
			tuiErr.Store(err)
		}
		pipeWriter.Close() // EOF → scanner goroutine exits
		p.Send(doneMsg{err: err})
	}()

	if _, err := p.Run(); err != nil {
		os.Stdout = origStdout
		os.Stderr = origStderr
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		os.Exit(1)
	}

	os.Stdout = origStdout
	os.Stderr = origStderr

	if v := tuiErr.Load(); v != nil {
		fmt.Fprintf(os.Stderr, "\n🚫 Fatal error: %v\n", v.(error))
		os.Exit(1)
	}

	if !tuiDone.Load() {
		fmt.Println("\n⏹️  Interrupted by user.")
		os.Exit(0)
	}

	fmt.Println("\n🎉 Script finished successfully.")
}
