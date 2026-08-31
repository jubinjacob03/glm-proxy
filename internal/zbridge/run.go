// Entry point of the Z.AI bridge.
//
// Run parses flags, opens the token database, starts the background workers,
// serves NewHandler() and blocks until SIGINT/SIGTERM, then drains in-flight
// requests and clears every still-pooled chat session.
//
// NewHandler is separate so integration tests can drive the whole HTTP surface
// without a listener.

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

// NewHandler assembles every route with auth and CORS applied.
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
	// otherwise hand internal state to anyone who can reach the port.
	mux.HandleFunc("/admin/stats", authMiddleware(statsHandler))
	mux.HandleFunc("/admin/health", authMiddleware(healthHandler))
	mux.HandleFunc("/admin/clients", authMiddleware(clientsHandler))
	mux.HandleFunc("/metrics", authMiddleware(metricsHandler))
	mux.HandleFunc("/inject.js", injectHandler)
	mux.HandleFunc("/stop", authMiddleware(stopHandler))

	return corsMiddleware(mux)
}

const backgroundDrain = 20 * time.Second

func startBackground(name string, run func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverGoroutine(name)
		run()
	}()
	return done
}

func waitForBackground(workers []<-chan struct{}, limit time.Duration) bool {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for _, done := range workers {
		select {
		case <-done:
		case <-timer.C:
			return false
		}
	}
	return true
}

// Run serves until a fatal error or a termination signal.
func Run() {
	flag.StringVar(&dbPath, "db-path", "tokens.sqlite", "Path to SQLite database")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose logging")
	flag.BoolVar(&config.AgentMode, "agent-mode", config.AgentMode, "Enable agent mode: translate tools & roles for Z.AI compatibility (native role-preserving passthrough by default)")
	flag.StringVar(&config.AgentModeVariant, "agent-mode-variant", config.AgentModeVariant, "Agent mode shim variant: native (default, role-preserving passthrough), modern (XML-sectioned fold), or legacy ([ROLE: ...] rewrite)")
	flag.BoolVar(&config.SyncMode, "sync-mode", config.SyncMode, "Legacy synchronous session flow: create a fresh chat per request instead of drawing from the pre-warmed session pool (used sessions are still deleted on Z.AI after each response)")
	flag.Parse()

	// Shorthand for LOG_LEVEL=debug.
	if verbose {
		config.Logging.Level = "debug"
		setLogLevel("debug")
	}

	log.SetOutput(NewLogWriter(config.Logging.ConsoleWidth))
	defer closeLogs()

	logInfof("Starting with db-path=%q log-level=%s", dbPath, config.Logging.Level)

	// initDB creates the schema when the file is new, so a fresh install starts
	// empty and the monitor fills it rather than the proxy refusing to boot.
	if err := initDB(); err != nil {
		// The store holds only harvested device tokens, which the monitor refills,
		// so a corrupt file is not worth refusing to start over: quarantine it and
		// retry once with a fresh one.
		if quarantineErr := quarantineTokenDB(); quarantineErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to open database: %v (could not quarantine: %v)\n", err, quarantineErr)
			os.Exit(1)
		}
		logConsolef("[Tokens] store was unreadable (%v); moved it aside and started fresh.", err)
		if err := initDB(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open database after quarantine: %v\n", err)
			os.Exit(1)
		}
	}
	defer closeDB()

	// Setup fills the store, so an empty one here means that step did not finish.
	if getTokenCount() == 0 {
		logConsolef("[Tokens] store is empty, so the monitor is collecting a batch now; " +
			"the first request waits for it rather than failing.")
	}

	// The shipped keys are public, so on a wildcard bind anyone who can reach the port
	// can spend this account's quota. Local-only clients do not need that exposure.
	if usingDefaultAuthToken() && isWildcardHost(config.Server.Host) {
		logConsolef("[Security] a built-in API key is accepted and the proxy is "+
			"listening on all interfaces, so anyone who can reach port %d can use "+
			"your Z.AI account. Set your own AUTH_TOKEN in .env, or HOST=127.0.0.1 to "+
			"accept local clients only.", config.Server.Port)
	}

	// A guest account cannot upload images or reach vision models, and the client
	// sees only a bare 401 or 403, so name the cause here.
	if config.ZaiToken == "" {
		logConsolef("[Session] ZAI_TOKEN is not set, so this runs as a guest. Guests cannot " +
			"upload images or use vision models, and those requests fail with 401/403. " +
			"Set it from the tray icon: right-click and choose Change token.")
	}

	gRunning.Store(true)

	// One cancel scope for every background worker, so shutdown actually stops
	// them instead of leaving them running past srv.Shutdown.
	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

	background := make([]<-chan struct{}, 0, 3)

	if config.TokenMonitor.Enabled {
		background = append(background, startBackground("token monitor", func() {
			tokenMonitor(bgCtx)
		}))
	}

	if config.AgentMode {
		background = append(background, startBackground("captcha cache", func() {
			captchaCache.Run(bgCtx)
		}))
		logInfof("Agent mode: captcha background cache started")
		switch {
		case config.agentNative():
			logInfof("Agent mode variant: NATIVE (role-preserving passthrough)")
		case config.agentModern():
			logInfof("Agent mode variant: MODERN (XML-sectioned prompt shim)")
		default:
			logInfof("Agent mode variant: LEGACY ([ROLE: ...] rewrite shim)")
		}
	}

	handler := NewHandler()

	addr := fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port)
	logBanner(startupBanner(false), startupBanner(true))

	// Tracked, so a shutdown inside the first few seconds does not close the log and
	// the database underneath it.
	background = append(background, startBackground("session init", func() {
		if err := initializeSession(); err != nil {
			logConsolef("[Startup] Session init deferred — will retry on first request.")
		}
		if bgCtx.Err() == nil {
			fetchModelsFromZAI()
		}
	}))

	// See session_pool.go: either mode runs each request on a throwaway chat
	// that is deleted upstream once its response is processed.
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
		// These two bound the parts that should never be slow, which closes the
		// Slowloris hole a server with no timeouts at all would leave open.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.Default(),
	}

	// Serve before blocking on signals.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	// Warm the batch in the background; requests queue on Acquire meanwhile.
	if sessionPool != nil {
		sessionPool.Start()
	}

	// SIGINT/SIGTERM stops accepting, drains in-flight responses, then clears
	// still-pooled sessions. A second signal force-exits, because stopSignal
	// re-arms default handling.
	ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Explicit, not deferred: log.Fatal would skip every defer and
			// abandon the DB handle, log buffer and pooled sessions.
			logErrorf("[Server] %v", err)
			gRunning.Store(false)
			stopBackground()
			if !waitForBackground(background, backgroundDrain) {
				logWarnf("[Shutdown] background workers exceeded %s", backgroundDrain)
			}
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

		// The cache waits for in-flight handshakes and the monitor kills its
		// collector child, rather than orphaning a browser.
		stopBackground()
		if !waitForBackground(background, backgroundDrain) {
			logWarnf("[Shutdown] background workers exceeded %s", backgroundDrain)
		}

		// Checked-out sessions go via their own Release; these are the leftovers.
		if sessionPool != nil {
			sessionPool.Shutdown()
		}
		// Those Releases delete upstream in the background. Exiting without waiting
		// would leave chats live on the account, which is what the deletes are for.
		waitForSessionGC(sessionGCDrain)
		logConsolef("[Shutdown] all chat sessions cleared. Goodbye.")
		flushLogs()
	}
}

// startupBanner renders the boxed startup summary. Widths are computed, so a long auth
// token cannot break the box; redacted masks that token for the log.txt copy.
func startupBanner(redacted bool) string {
	authToken := authTokenDisplay(redacted)
	rows := [][2]string{
		{"Listening", addrForDisplay()},
		{"Health", fmt.Sprintf("http://localhost:%d/health", config.Server.Port)},
		{"OpenAI API", fmt.Sprintf("http://localhost:%d/v1/chat/completions", config.Server.Port)},
		{"Anthropic API", fmt.Sprintf("http://localhost:%d/v1/messages", config.Server.Port)},
		{"Auth token", authToken},
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

	// Lay out every row first, then size the box to the longest. Rows used to be
	// padded to two widths depending on whether the value held a URL, which left
	// the link rows short and the box permanently open.
	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, title)
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%-*s  %s", labelWidth, r[0], r[1]))
	}

	width := 0
	for _, l := range lines {
		if n := len([]rune(l)); n > width {
			width = n
		}
	}

	// A row is "|  " + text + pad + "|", a border is "+" + dashes + "+".
	// dashes = width+4 gives two spaces either side and equal total length.
	dashes := width + 4

	var b strings.Builder
	b.Grow((dashes + 4) * (len(lines) + 4))

	border := func() {
		b.WriteByte('+')
		b.WriteString(strings.Repeat("-", dashes))
		b.WriteString("+\n")
	}
	row := func(text string) {
		pad := dashes - 2 - len([]rune(text))
		if pad < 0 {
			pad = 0
		}
		b.WriteString("|  ")
		b.WriteString(text)
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString("|\n")
	}

	b.WriteByte('\n')
	border()
	row(lines[0])
	border()
	for _, l := range lines[1:] {
		row(l)
	}
	border()
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
	case config.agentNative():
		return "on (native)"
	case config.agentModern():
		return "on (modern shim)"
	case config.agentLegacy():
		return "on (legacy shim)"
	default:
		return "off"
	}
}

// resolveCollectorPath finds the collector exe, honouring the platform extension
// and falling back to PATH.
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
	// Beside our own exe, so the working directory does not matter.
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

// tokenMonitor keeps the store stocked. Every verification spends one token, so
// an empty store fails every completion.
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

	// Requests may now wait on this loop instead of failing, so advertise that it
	// exists and is working.
	collectorHealthy.Store(true)
	defer collectorHealthy.Store(false)

	// Consecutive failures back the loop off, so a collector that cannot succeed
	// (bad credentials, no browser, locked database) is not relaunched forever.
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
				collectorHealthy.Store(false)
				metrics.collectorFailures.Add(1)
				logErrorf("[Tokens] collector failed (%d consecutive): %v", failures, err)
			} else {
				failures = 0
				collectorHealthy.Store(true)
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

		// While the collector succeeds, a request that hit an empty store can cut
		// the wait short. A nil channel blocks forever in select, so once runs
		// fail the backoff is honoured instead of bypassed by every request.
		var demand <-chan struct{}
		if failures == 0 {
			demand = tokenDemand
		}

		select {
		case <-ctx.Done():
			return
		case <-demand:
			logConsolef("[Tokens] a request is waiting on tokens; collecting now")
		case <-time.After(wait):
		}
	}
}

// collectorRunning keeps two collectors off the same SQLite file: the loop cannot
// overlap itself, but a manual run can.
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

	// No --fresh: the store still holds usable tokens, and the collector appends.
	cmd := exec.CommandContext(runCtx, collector,
		"--no-tui",
		"--batch", strconv.Itoa(config.TokenMonitor.Batch),
		"--db-path", dbAbs,
	)
	cmd.Dir = filepath.Dir(collector)
	// Killed on cancellation rather than orphaning a Playwright browser tree,
	// with a grace period to close the database cleanly.
	cmd.WaitDelay = 10 * time.Second

	// Line by line for visible progress, plus a bounded tail for error context.
	progress := &collectorProgress{limit: 8 << 10}
	cmd.Stdout = progress
	cmd.Stderr = progress

	err = cmd.Run()
	progress.close()
	if err != nil {
		if tail := progress.tail(); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

// collectorProgress forwards the collector's output line by line, so a first run
// shows progress in Monitor instead of looking idle while a headless browser
// works. The bounded tail lets a failure say more than its exit status without
// letting a chatty child grow the buffer without limit.
type collectorProgress struct {
	mu    sync.Mutex
	line  strings.Builder
	kept  strings.Builder
	limit int
}

func (c *collectorProgress) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if room := c.limit - c.kept.Len(); room > 0 {
		if len(p) > room {
			c.kept.Write(p[:room])
		} else {
			c.kept.Write(p)
		}
	}

	for _, b := range p {
		switch b {
		case '\n':
			c.emitLocked()
		case '\r':
			// The collector redraws with carriage returns, so treat the segment
			// as complete rather than gluing the next one on.
			c.emitLocked()
		default:
			c.line.WriteByte(b)
		}
	}
	return len(p), nil
}

func (c *collectorProgress) emitLocked() {
	text := strings.TrimSpace(c.line.String())
	c.line.Reset()
	if informativeLine(text) {
		logConsolef("[Collector] %s", text)
	}
}

// close flushes a trailing line that never got its newline.
func (c *collectorProgress) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.emitLocked()
}

func (c *collectorProgress) tail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.kept.String())
}

// informativeLine rejects the collector's decorative rules, built from one
// repeated character and worth no log line.
func informativeLine(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch r {
		case '=', '-', '_', '~', '*', ' ', '═', '─', '━', '┈':
		default:
			return true
		}
	}
	return false
}
