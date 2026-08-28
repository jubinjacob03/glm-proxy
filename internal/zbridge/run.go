// Entry point of the Z.AI bridge (package zbridge).
// Entry point of the Z.AI bridge.
//
// Run parses the CLI flags, opens the token database, starts the background
// workers, serves NewHandler() and blocks until SIGINT/SIGTERM, then drains
// in-flight requests and clears every still-pooled chat session on Z.AI.
//
// NewHandler is exported separately so integration tests can drive the full
// HTTP surface without starting a listener.

package zbridge

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// NewHandler assembles the bridge's complete HTTP surface: every route with
// the auth and CORS middleware applied.
func NewHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", dashboardHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/v1/models", authMiddleware(modelsHandler))
	mux.HandleFunc("/models", authMiddleware(modelsHandler2))
	mux.HandleFunc("/v1/chat/completions", authMiddleware(chatCompletionsHandler))
	mux.HandleFunc("/v1/messages", authMiddleware(anthropicMessagesHandler))
	mux.HandleFunc("/features", authMiddleware(featuresHandler))
	// Authenticated: the listener binds 0.0.0.0 by default, so these would
	// otherwise expose internal state to anyone who can reach the port.
	mux.HandleFunc("/admin/stats", authMiddleware(statsHandler))
	mux.HandleFunc("/admin/health", authMiddleware(healthHandler))
	mux.HandleFunc("/admin/clients", authMiddleware(clientsHandler))
	mux.HandleFunc("/metrics", authMiddleware(metricsHandler))
	mux.HandleFunc("/inject.js", injectHandler)
	mux.HandleFunc("/stop", authMiddleware(stopHandler))

	return corsMiddleware(mux)
}

// Run starts the bridge server and blocks until a fatal error or a
// termination signal. Called from the root package's main().
func Run() {
	flag.StringVar(&dbPath, "db-path", "tokens.sqlite", "Path to SQLite database")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose logging")
	flag.BoolVar(&config.AgentMode, "agent-mode", config.AgentMode, "Enable agent mode: translate tools & roles for Z.AI compatibility (modern shim by default)")
	flag.StringVar(&config.AgentModeVariant, "agent-mode-variant", config.AgentModeVariant, "Agent mode shim variant: modern (default, XML-sectioned prompt) or legacy ([ROLE: ...] rewrite)")
	flag.BoolVar(&config.SyncMode, "sync-mode", config.SyncMode, "Legacy synchronous session flow: create a fresh chat per request instead of drawing from the pre-warmed session pool (used sessions are still deleted on Z.AI after each response)")
	flag.Parse()

	// --verbose is shorthand for LOG_LEVEL=debug.
	if verbose {
		config.Logging.Level = "debug"
		setLogLevel("debug")
	}

	log.SetOutput(NewLogWriter(config.Logging.ConsoleWidth))
	defer closeLogs()

	logInfof("Starting with db-path=%q log-level=%s", dbPath, config.Logging.Level)

	// initDB creates the token schema if the file is new, so a fresh install
	// starts with an empty store and the token monitor fills it in the
	// background rather than the proxy refusing to start.
	if err := initDB(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer closeDB()

	if getTokenCount() == 0 {
		logConsolef("[Tokens] store is empty; the monitor will collect the first batch now (first run downloads a browser, ~1-2 min).")
	}

	gRunning.Store(true)

	// One cancel scope for all background workers, so shutdown stops them
	// rather than leaving them running past srv.Shutdown.
	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

	var background sync.WaitGroup

	if config.TokenMonitor.Enabled {
		background.Add(1)
		go func() {
			defer background.Done()
			tokenMonitor(bgCtx)
		}()
	}

	if config.AgentMode {
		background.Add(1)
		go func() {
			defer background.Done()
			captchaCache.Run(bgCtx)
		}()
		logInfof("Agent mode: captcha background cache started")
		if config.agentModern() {
			logInfof("Agent mode variant: MODERN (XML-sectioned prompt shim)")
		} else {
			logInfof("Agent mode variant: LEGACY ([ROLE: ...] rewrite shim)")
		}
	}

	handler := NewHandler()

	addr := fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port)
	logBanner(startupBanner())

	go func() {
		if err := initializeSession(); err != nil {
			logConsolef("[Startup] Session init deferred — will retry on first request.")
		}
		fetchModelsFromZAI() // warm the model cache
	}()

	// Every request runs on a throwaway chat session, deleted on Z.AI once its
	// response is processed, so the account never accumulates dead sessions.
	// The async flow keeps a standing batch ready; --sync-mode mints one per
	// request instead. Either way they are garbage-collected.
	if config.SyncMode {
		logInfof("[Startup] Session mode: SYNC (fresh chat per request, deleted after use)")
	} else {
		poolWait = time.Duration(config.SessionAcquireTimeout) * time.Second
		if config.SessionAcquireTimeout <= 0 {
			poolWait = 0 // 0 => wait indefinitely for a pooled session
		}
		sessionPool = NewSessionPool(NewZAIChatBackend(), config.SessionPoolSize)
		logInfof("[Startup] Session mode: ASYNC (pre-made batch x%d, deleted and refilled after each response)", sessionPool.Size())
		logInfof("[Startup] SESSION_POOL_SIZE=%d SESSION_ACQUIRE_TIMEOUT=%ds", sessionPool.Size(), config.SessionAcquireTimeout)
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// WriteTimeout must stay unset: responses are long-lived SSE streams.
		// These two bound the parts that should never be slow, closing the
		// Slowloris exposure of a server with no timeouts at all.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.Default(),
	}

	// Start serving before blocking on signals.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	// Warm the standing session batch in the background; requests are served
	// meanwhile (they simply queue on Acquire until sessions appear).
	if sessionPool != nil {
		sessionPool.Start()
	}

	// SIGINT/SIGTERM stops accepting connections, lets in-flight responses
	// finish within the drain deadline, then clears every still-pooled chat
	// session on Z.AI. A second signal force-exits, since default handling is
	// re-armed by stopSignal.
	ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Cleanup is explicit rather than deferred: log.Fatal would skip
			// every defer and abandon the database handle, the log buffer and
			// the pooled chat sessions.
			logErrorf("[Server] %v", err)
			gRunning.Store(false)
			stopBackground()
			background.Wait()
			if sessionPool != nil {
				sessionPool.Shutdown()
			}
			closeDB()
			closeLogs()
			os.Exit(1)
		}
	case <-ctx.Done():
		stopSignal()
		gRunning.Store(false)
		logConsolef("[Shutdown] draining connections and clearing chat sessions...")

		drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := srv.Shutdown(drainCtx); err != nil {
			logWarnf("[Shutdown] drain deadline hit (%v); closing remaining connections", err)
			_ = srv.Close()
		}
		cancel()

		// The captcha cache waits for in-flight handshakes and the token
		// monitor kills its collector child, rather than orphaning a browser.
		stopBackground()
		background.Wait()

		// Checked-out sessions are deleted by their own Release; these are the
		// ones still sitting in the pool.
		if sessionPool != nil {
			sessionPool.Shutdown()
		}
		logConsolef("[Shutdown] all chat sessions cleared. Goodbye.")
		flushLogs()
	}
}

// ============================================================================
// DEVICE TOKEN REPLENISHMENT
// ============================================================================

// startupBanner renders the boxed summary printed once at startup. Widths are
// computed rather than hardcoded, so a 4-digit port or a long auth token cannot
// break the box the way the fixed padding used to.
func startupBanner() string {
	rows := [][2]string{
		{"Listening", addrForDisplay()},
		{"Health", fmt.Sprintf("http://localhost:%d/health", config.Server.Port)},
		{"OpenAI API", fmt.Sprintf("http://localhost:%d/v1/chat/completions", config.Server.Port)},
		{"Anthropic API", fmt.Sprintf("http://localhost:%d/v1/messages", config.Server.Port)},
		{"Auth token", config.Auth.Token},
		{"Agent mode", agentModeLabel()},
		{"Log level", fmt.Sprintf("%s (console shows one line per request)", config.Logging.Level)},
		{"Log file", filepath.Join(config.Logging.Dir, "log.txt")},
	}

	const title = "GLM Proxy by J - Z.AI bridge ready"
	labelWidth := 0
	for _, r := range rows {
		if n := len([]rune(r[0])); n > labelWidth {
			labelWidth = n
		}
	}
	inner := len([]rune(title))
	for _, r := range rows {
		if n := labelWidth + 2 + len([]rune(r[1])); n > inner {
			inner = n
		}
	}
	inner += 4 // two spaces of padding either side

	const linkIconPad = 3
	outer := inner + linkIconPad

	var b strings.Builder
	line := func() {
		b.WriteString("+")
		b.WriteString(strings.Repeat("-", outer))
		b.WriteString("+\n")
	}
	row := func(text string, hasLink bool) {
		target := outer
		if hasLink {
			target = inner
		}
		pad := target - 2 - len([]rune(text))
		if pad < 0 {
			pad = 0
		}
		b.WriteString("|  ")
		b.WriteString(text)
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString("|\n")
	}

	b.WriteByte('\n')
	line()
	row(title, false)
	line()
	for _, r := range rows {
		row(fmt.Sprintf("%-*s  %s", labelWidth, r[0], r[1]), strings.Contains(r[1], "http"))
	}
	line()
	return b.String()
}

func addrForDisplay() string {
	host := config.Server.Host
	if host == "0.0.0.0" || host == "" {
		host = "0.0.0.0 (all interfaces)"
	}
	return fmt.Sprintf("%s:%d", host, config.Server.Port)
}

func agentModeLabel() string {
	switch {
	case config.agentModern():
		return "on (modern shim)"
	case config.agentLegacy():
		return "on (legacy shim)"
	default:
		return "off"
	}
}

// resolveCollectorPath locates the token-collector executable, honouring the
// platform's extension and falling back to PATH.
func resolveCollectorPath() (string, bool) {
	if p := config.TokenMonitor.CollectorPath; p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs, true
			}
		}
		return "", false
	}

	name := "token-collector"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	candidates := []string{name}
	// Alongside our own executable, so the working directory does not matter.
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), name))
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			return abs, true
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	return "", false
}

// tokenMonitor keeps the device token store stocked. Every captcha
// verification spends one token, so an empty store fails every completion.
func tokenMonitor(ctx context.Context) {
	collector, ok := resolveCollectorPath()
	if !ok {
		logErrorf("token monitor disabled: token-collector executable not found " +
			"(build it with `go build -o token-collector ./cmd/token-collector`, " +
			"or set TOKEN_COLLECTOR_PATH)")
		return
	}
	logConsolef("[Tokens] monitor active: collector=%s threshold=%d interval=%s",
		collector, config.TokenMonitor.MinTokens, config.TokenMonitor.Interval)

	// Consecutive failures back the loop off, so a collector that cannot
	// succeed (expired credentials, no browser, locked database) is not
	// relaunched at full rate forever.
	failures := 0

	for {
		count := getTokenCount()
		if count < config.TokenMonitor.MinTokens {
			logConsolef("[Tokens] low water mark: %d < %d, running collector",
				count, config.TokenMonitor.MinTokens)

			if err := runTokenCollector(ctx, collector); err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				metrics.collectorFailures.Add(1)
				logErrorf("[Tokens] collector failed (%d consecutive): %v", failures, err)
			} else {
				failures = 0
				metrics.collectorRuns.Add(1)
				metrics.collectorLastRun.Store(time.Now().Unix())
				logConsolef("[Tokens] collector finished; %d tokens available", refreshTokenCount())
			}
		} else {
			failures = 0
		}

		wait := config.TokenMonitor.Interval
		for i := 0; i < failures && wait < 30*time.Minute; i++ {
			wait *= 2
		}
		if wait != config.TokenMonitor.Interval {
			logWarnf("[Tokens] backing off for %s after %d failures", wait, failures)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// collectorRunning keeps two collector processes off the same SQLite file. The
// monitor loop cannot overlap itself, but a manual run can.
var collectorRunning atomic.Bool

func runTokenCollector(ctx context.Context, collector string) error {
	if !collectorRunning.CompareAndSwap(false, true) {
		return errors.New("a collector run is already in progress")
	}
	defer collectorRunning.Store(false)

	runCtx, cancel := context.WithTimeout(ctx, config.TokenMonitor.RunTimeout)
	defer cancel()

	dbAbs, err := filepath.Abs(dbPath)
	if err != nil {
		dbAbs = dbPath
	}

	// No --fresh: the store still holds usable tokens while this runs, and the
	// collector appends by default.
	cmd := exec.CommandContext(runCtx, collector,
		"--no-tui",
		"--batch", strconv.Itoa(config.TokenMonitor.Batch),
		"--db-path", dbAbs,
	)
	cmd.Dir = filepath.Dir(collector)
	// Killed on cancellation rather than left as an orphaned Playwright browser
	// tree, with a grace period to close the database cleanly first.
	cmd.WaitDelay = 10 * time.Second

	// Captured so a failing collector is not silent.
	var output strings.Builder
	cmd.Stdout = &boundedWriter{dst: &output, limit: 8 << 10}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(output.String()); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	if debugEnabled() {
		logDebugf("[Tokens] collector output: %s", strings.TrimSpace(output.String()))
	}
	return nil
}

// boundedWriter keeps at most limit bytes, so a chatty child cannot grow the
// buffer without bound.
type boundedWriter struct {
	dst   *strings.Builder
	limit int
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if room := b.limit - b.dst.Len(); room > 0 {
		if len(p) > room {
			b.dst.Write(p[:room])
		} else {
			b.dst.Write(p)
		}
	}
	return len(p), nil
}
