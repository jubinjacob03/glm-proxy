package zbridge

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"zai-api/internal/ansi"
)

// defaultAuthTokens ship in the repo, so their presence is worth warning about.
var defaultAuthTokens = []string{"Jubin", "unlimited"}

func isWildcardHost(h string) bool {
	return h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" || h == "*"
}

// usingDefaultAuthToken reports whether any shipped key is still accepted.
func usingDefaultAuthToken() bool {
	for _, tok := range config.Auth.Tokens {
		for _, def := range defaultAuthTokens {
			if tok == def {
				return true
			}
		}
	}
	return false
}

// authTokenDisplay renders the accepted keys for the startup banner, masking each
// for the log-file copy since a custom key may be a real secret.
func authTokenDisplay(redacted bool) string {
	toks := make([]string, 0, len(config.Auth.Tokens))
	for _, t := range config.Auth.Tokens {
		if redacted {
			t = redactSecret(t)
		}
		toks = append(toks, t)
	}
	return strings.Join(toks, " or ")
}

const (
	logLevelDebug int32 = iota
	logLevelInfo
	logLevelWarn
	logLevelError
	logLevelOff
)

// An atomic int, not a string compare against config: read once per SSE line.
var logLevel atomic.Int32

func parseLogLevel(s string) int32 {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "trace", "verbose":
		return logLevelDebug
	case "warn", "warning":
		return logLevelWarn
	case "error", "err":
		return logLevelError
	case "off", "none", "silent", "quiet":
		return logLevelOff
	default:
		return logLevelInfo
	}
}

func setLogLevel(s string) { logLevel.Store(parseLogLevel(s)) }

func debugEnabled() bool { return logLevel.Load() <= logLevelDebug }
func infoEnabled() bool  { return logLevel.Load() <= logLevelInfo }
func warnEnabled() bool  { return logLevel.Load() <= logLevelWarn }
func errorEnabled() bool { return logLevel.Load() <= logLevelError }

// Console and file carry different detail on purpose. The file gets everything at
// the configured level; the console gets only what an operator watching needs —
// banner, one line per request, warn and above. LOG_LEVEL=debug mirrors the file.
//
// Records reach the file through the standard log package, so existing log.Printf
// sites keep working and stay off the console. Lines that must be seen go through
// logConsolef, which is why it bypasses log.Printf.

func logDebugf(format string, args ...interface{}) {
	if debugEnabled() {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func logWarnf(format string, args ...interface{}) {
	if warnEnabled() {
		log.Printf("[WARN] "+format, args...)
	}
}

// logInfof records operational detail: file only, unless the level is debug.
func logInfof(format string, args ...interface{}) {
	if infoEnabled() {
		log.Printf(format, args...)
	}
}

func logErrorf(format string, args ...interface{}) {
	if errorEnabled() {
		log.Printf("[ERROR] "+format, args...)
	}
}

// logConsolef records what the operator should see — lifecycle, token
// replenishment, one summary per request — always to both destinations.
func logConsolef(format string, args ...interface{}) {
	if logLevel.Load() >= logLevelOff {
		return
	}
	line := fmt.Sprintf(format, args...)
	if lw := globalLogWriter.Load(); lw != nil {
		lw.emit(line)
		return
	}
	fmt.Println(line)
}

// logBanner writes pre-formatted output untimestamped and untruncated, so box drawing
// survives. The texts differ only where the console may show a secret log.txt should
// not keep: log files get pasted into bug reports.
func logBanner(console, file string) {
	if lw := globalLogWriter.Load(); lw != nil {
		lw.writeRaw(console, file)
		return
	}
	fmt.Print(console)
}

// The writer itself: full lines to a rotating file, truncated to the console.

const truncationMarker = "... [TRUNCATED]"

// LogWriter is installed on the standard log package. Lines go to a size-rotated
// file in full and to the console truncated, so streaming payloads stay readable
// without losing anything on disk.
//
// The file side is buffered and ticker-flushed because lumberjack issues one
// syscall per Write, and at debug level this is hit once per SSE line.
type LogWriter struct {
	mu      sync.Mutex
	rotator *lumberjack.Logger
	buf     *bufio.Writer
	console *os.File
	// scratch holds the truncated console line, stamp the timestamped one, so the
	// common case does not allocate per record.
	scratch   []byte
	stamp     []byte
	maxLength int
	dirty     bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

var globalLogWriter atomic.Pointer[LogWriter]

// NewLogWriter builds the process log writer. MaxSize is the only rotation
// trigger (lumberjack has no timer); MaxBackups and MaxAge bound retention.
func NewLogWriter(maxStdoutLen int) *LogWriter {
	dir := config.Logging.Dir
	if dir == "" {
		dir = "logs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "Warning: failed to create log directory:", err)
	}

	if maxStdoutLen <= 0 {
		maxStdoutLen = 150
	}

	rotator := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "log.txt"),
		MaxSize:    config.Logging.MaxSizeMB,
		MaxBackups: config.Logging.MaxBackups,
		MaxAge:     config.Logging.MaxAgeDays,
		Compress:   true,
	}

	lw := &LogWriter{
		rotator:   rotator,
		buf:       bufio.NewWriterSize(rotator, 64*1024),
		console:   os.Stdout,
		scratch:   make([]byte, 0, maxStdoutLen+len(truncationMarker)+1),
		maxLength: maxStdoutLen,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}

	go lw.flushLoop(time.Duration(config.Logging.FlushMillis) * time.Millisecond)
	globalLogWriter.Store(lw)
	return lw
}

func (lw *LogWriter) flushLoop(interval time.Duration) {
	defer close(lw.done)
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			lw.Flush()
		case <-lw.stop:
			lw.Flush()
			return
		}
	}
}

// Flush drains the buffered file writer; cheap when nothing is pending.
func (lw *LogWriter) Flush() {
	lw.mu.Lock()
	if lw.dirty {
		if err := lw.buf.Flush(); err != nil {
			fmt.Fprintln(os.Stderr, "log flush failed:", err)
		}
		lw.dirty = false
	}
	lw.mu.Unlock()
}

// Close flushes and stops the background flusher; safe to call repeatedly.
func (lw *LogWriter) Close() {
	lw.stopOnce.Do(func() {
		close(lw.stop)
		<-lw.done
		lw.mu.Lock()
		_ = lw.buf.Flush()
		lw.dirty = false
		lw.mu.Unlock()
		_ = lw.rotator.Close()
	})
}

// Write implements io.Writer for the log package. Everything reaches the file;
// the console sees a record only when it earns it, which is what stops a
// streaming workload burying the terminal.
//
// The log package serialises its own callers; the mutex guards against anything
// else sharing this writer interleaving a line.
func (lw *LogWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	lw.toFile(p)
	if consoleWorthy(p) {
		lw.toConsole(p)
	}

	// Report the caller's length, so the log package sees a complete write.
	return len(p), nil
}

// consoleWorthy reports whether a log-package record also belongs on screen.
func consoleWorthy(p []byte) bool {
	if debugEnabled() {
		return true // debug mirrors the file
	}
	return bytes.Contains(p, []byte("[ERROR]")) || bytes.Contains(p, []byte("[WARN]"))
}

// emit writes one timestamped line to both destinations.
func (lw *LogWriter) emit(line string) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	lw.stamp = lw.stamp[:0]
	lw.stamp = time.Now().AppendFormat(lw.stamp, "2006/01/02 15:04:05 ")
	lw.stamp = append(lw.stamp, line...)
	if len(line) == 0 || line[len(line)-1] != '\n' {
		lw.stamp = append(lw.stamp, '\n')
	}

	lw.toFile(lw.stamp)
	lw.toConsole(lw.stamp)
}

// writeRaw sends text verbatim to both destinations.
func (lw *LogWriter) writeRaw(console, file string) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	// Stripped like every other file write, so the log.txt promise holds even if a
	// banner is ever coloured.
	if _, err := lw.buf.Write(ansi.StripBytes([]byte(file))); err == nil {
		lw.dirty = true
	}
	_, _ = io.WriteString(lw.console, console)
}

// toFile writes p without colour, so escape codes never reach log.txt.
func (lw *LogWriter) toFile(p []byte) {
	if _, err := lw.buf.Write(ansi.StripBytes(p)); err == nil {
		lw.dirty = true
	}
}

// toConsole writes p truncated to the console budget and coloured.
func (lw *LogWriter) toConsole(p []byte) {
	out := p
	// The newline is not part of the width: a line that exactly fills the budget
	// would otherwise be stamped as truncated.
	measured := p
	if n := len(measured); n > 0 && measured[n-1] == '\n' {
		measured = measured[:n-1]
	}
	if ansi.VisibleLen(measured) > lw.maxLength {
		lw.scratch = ansi.TruncateVisible(lw.scratch[:0], p, lw.maxLength)
		lw.scratch = append(lw.scratch, truncationMarker...)
		if p[len(p)-1] == '\n' {
			lw.scratch = append(lw.scratch, '\n')
		}
		out = lw.scratch
	}
	_, _ = lw.console.Write(colourPrefix(out))
}

// tagColour maps a bracketed prefix to its colour; unlisted tags fall back to cyan.
var tagColour = map[string]func(string) string{
	"[ERROR]":     ansi.Red,
	"[WARN]":      ansi.Yellow,
	"[DEBUG]":     ansi.Grey,
	"[Collector]": ansi.Violet,
	"[Vision]":    ansi.Violet,
	"[Image]":     ansi.Violet,
	"[Pool]":      ansi.Blue,
	"[Startup]":   ansi.Grey,
	"[Shutdown]":  ansi.Grey,
}

// colourPrefix greys the leading timestamp and colours the tag behind it. Only the
// prefix is touched, so colour a caller applied to the rest of the line survives.
func colourPrefix(p []byte) []byte {
	if !ansi.Enabled {
		return p
	}
	rest := p
	var out []byte
	if n := timestampWidth(p); n > 0 {
		out = append(out, ansi.Grey(string(p[:n]))...)
		rest = p[n:]
	}
	if len(rest) > 0 && rest[0] == '[' {
		if end := bytes.IndexByte(rest, ']'); end > 0 {
			tag := string(rest[:end+1])
			paint, ok := tagColour[tag]
			if !ok {
				paint = ansi.Cyan
			}
			out = append(out, paint(tag)...)
			rest = rest[end+1:]
		}
	}
	if out == nil {
		return p
	}
	return append(out, rest...)
}

// timestampWidth is the length of a leading "2006/01/02 15:04:05 " stamp, else zero.
func timestampWidth(p []byte) int {
	const width = len("2006/01/02 15:04:05 ")
	if len(p) < width {
		return 0
	}
	if p[4] == '/' && p[7] == '/' && p[10] == ' ' && p[13] == ':' && p[16] == ':' && p[19] == ' ' {
		return width
	}
	return 0
}

func flushLogs() {
	if lw := globalLogWriter.Load(); lw != nil {
		lw.Flush()
	}
}

func closeLogs() {
	if lw := globalLogWriter.Load(); lw != nil {
		lw.Close()
	}
}
