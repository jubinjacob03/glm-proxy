package zbridge

import (
	"strconv"
	"strings"
	"time"

	"zai-api/internal/ansi"
)

// One console line per request, so a busy terminal stays readable. The per-chunk
// detail behind each line still reaches the log file at info or debug level.

// accessRecord accumulates the summary line. Handlers create one on entry and
// defer done, so every exit path is covered.
type accessRecord struct {
	started  time.Time
	method   string
	path     string
	model    string
	stream   bool
	status   int
	outcome  string // "" when normal, else a short reason
	bytesOut int64
	chunks   int
	reported bool
}

func newAccessRecord(method, path string) *accessRecord {
	return &accessRecord{
		started: time.Now(),
		method:  method,
		path:    path,
		status:  200,
	}
}

func (a *accessRecord) fail(status int, reason string) {
	a.status = status
	a.outcome = reason
}

// done emits the summary; only the first call wins.
func (a *accessRecord) done() {
	if a == nil || a.reported {
		return
	}
	a.reported = true

	// Labels greyed, values bright: the eye lands on model, status and timing.
	var b strings.Builder
	b.Grow(256)
	b.WriteString(ansi.Bold(ansi.Violet(printableASCII(a.method))))
	b.WriteByte(' ')
	b.WriteString(printableASCII(a.path))

	if a.model != "" {
		b.WriteString(ansi.Grey(" model="))
		b.WriteString(ansi.Cyan(printableASCII(a.model)))
	}
	if a.stream {
		b.WriteString(ansi.Grey(" stream"))
	}

	paint := statusColour(a.status)
	b.WriteByte(' ')
	b.WriteString(ansi.Bold(paint(strconv.Itoa(a.status))))

	elapsed := time.Since(a.started)
	b.WriteString(ansi.Grey(" in "))
	b.WriteString(formatDuration(elapsed))

	if a.chunks > 0 {
		b.WriteString(ansi.Grey(" chunks="))
		b.WriteString(strconv.Itoa(a.chunks))
	}
	if a.bytesOut > 0 {
		b.WriteString(ansi.Grey(" out="))
		b.WriteString(formatBytes(a.bytesOut))
	}
	if a.outcome != "" {
		b.WriteByte(' ')
		b.WriteString(paint("(" + printableASCII(a.outcome) + ")"))
	}

	logConsolef("%s", b.String())
}

func statusColour(status int) func(string) string {
	switch {
	case status >= 500:
		return ansi.Red
	case status >= 400:
		return ansi.Yellow
	case status >= 200 && status < 300:
		return ansi.Green
	default:
		return ansi.Cyan
	}
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return strconv.FormatInt(int64(d/time.Microsecond), 10) + "us"
	case d < time.Second:
		return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
	default:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	}
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	value := float64(n)
	units := [...]string{"KB", "MB", "GB"}
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + units[idx]
}
