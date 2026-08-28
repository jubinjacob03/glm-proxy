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
	"unicode/utf8"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// ============================================================================
// LOG LEVELS
// ============================================================================

const (
	logLevelDebug int32 = iota
	logLevelInfo
	logLevelWarn
	logLevelError
	logLevelOff
)

// An atomic int rather than a string compare against config, because this is
// read once per upstream SSE line.
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

// ============================================================================
// CONSOLE VS FILE
// ============================================================================
//
// The terminal and the log file carry different amounts of detail on purpose.
// The file gets everything at the configured level. The console gets only what
// an operator watching the window needs: the startup banner, one line per
// request, and anything at warn or above. At LOG_LEVEL=debug the console opens
// up and mirrors the file.
//
// Records reach the file through the standard log package, so the ~30 existing
// log.Printf call sites keep working and stay off the console. Lines that must
// be visible go through logConsolef, which is why it does not use log.Printf.

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

// logInfof records operational detail. It lands in the log file but stays off
// the console unless the level is debug.
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

// logConsolef records a line the operator should see: lifecycle events, token
// replenishment, one summary per request. Always written to both destinations.
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

// logBanner writes pre-formatted multi-line output to both destinations without
// the per-line timestamp or the console truncation, so box drawing survives.
func logBanner(text string) {
	if lw := globalLogWriter.Load(); lw != nil {
		lw.writeRaw(text)
		return
	}
	fmt.Print(text)
}

// ============================================================================
// LOG WRITER — truncated console + buffered rotating file
// ============================================================================

const truncationMarker = "... [TRUNCATED]"

// LogWriter is the destination installed on the standard log package. Every
// line goes to a size-rotated file in full and to the console truncated, so
// streaming AI payloads stay readable in the terminal without losing anything
// on disk.
//
// The file side is buffered and flushed on a ticker because lumberjack issues
// one write syscall per Write, and at debug level this writer is hit once per
// upstream SSE line.
type LogWriter struct {
	mu      sync.Mutex
	rotator *lumberjack.Logger
	buf     *bufio.Writer
	console *os.File
	// scratch holds the truncated console line and stamp the timestamped one,
	// so the common case does not allocate per log record.
	scratch   []byte
	stamp     []byte
	maxLength int
	dirty     bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

var globalLogWriter atomic.Pointer[LogWriter]

// NewLogWriter builds the process log writer. Rotation is size-triggered only;
// lumberjack has no timer, so MaxSize is the sole trigger and
// MaxBackups/MaxAge bound retention.
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

// Flush drains the buffered file writer. Cheap when nothing is pending.
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

// Close flushes and stops the background flusher. Safe to call more than once.
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

// Write implements io.Writer for the standard log package. Everything reaches
// the file; the console sees the record only when it is important enough, which
// is what keeps a streaming workload from burying the terminal.
//
// The standard log package already serialises its own callers; the mutex
// protects against anything else sharing this writer interleaving a line.
func (lw *LogWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	lw.toFile(p)
	if consoleWorthy(p) {
		lw.toConsole(p)
	}

	// The caller's length, so the log package sees a complete write.
	return len(p), nil
}

// consoleWorthy reports whether a record formatted by the log package should
// also surface on the console.
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

// writeRaw sends text to both destinations verbatim: no timestamp and no
// truncation, so multi-line box drawing stays intact.
func (lw *LogWriter) writeRaw(text string) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	if _, err := lw.buf.WriteString(text); err == nil {
		lw.dirty = true
	}
	_, _ = io.WriteString(lw.console, text)
}

func (lw *LogWriter) toFile(p []byte) {
	if _, err := lw.buf.Write(p); err == nil {
		lw.dirty = true
	}
}

// toConsole writes p truncated to the console budget.
func (lw *LogWriter) toConsole(p []byte) {
	out := p
	if len(p) > lw.maxLength {
		// Cut on a rune boundary: log bodies carry CJK and emoji from Z.AI, and
		// slicing mid-rune renders as U+FFFD on the console.
		cut := lw.maxLength
		for cut > 0 && !utf8.RuneStart(p[cut]) {
			cut--
		}
		lw.scratch = append(lw.scratch[:0], p[:cut]...)
		lw.scratch = append(lw.scratch, truncationMarker...)
		if p[len(p)-1] == '\n' {
			lw.scratch = append(lw.scratch, '\n')
		}
		out = lw.scratch
	}
	_, _ = lw.console.Write(out)
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
