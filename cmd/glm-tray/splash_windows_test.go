//go:build windows

package main

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSplashSetKeepsLatestState(t *testing.T) {
	s := &splash{view: splashView{percent: splashIndeterminate, text: "initial"}}
	s.set(42, "Downloading 更新")
	if got := s.snapshot(); got != (splashView{percent: 42, text: "Downloading 更新"}) {
		t.Fatalf("snapshot = %#v", got)
	}
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.set(99, "late")
	if got := s.snapshot(); got != (splashView{percent: 42, text: "Downloading 更新"}) {
		t.Fatalf("set after close changed state to %#v", got)
	}
}

func TestClampSplashPercent(t *testing.T) {
	cases := []struct {
		input int
		want  int
	}{
		{-1, 0},
		{0, 0},
		{42, 42},
		{100, 100},
		{101, 100},
	}
	for _, test := range cases {
		if got := clampSplashPercent(test.input); got != test.want {
			t.Errorf("clampSplashPercent(%d) = %d, want %d", test.input, got, test.want)
		}
	}
}

func TestEaseSplashProgressIsFrameRateIndependent(t *testing.T) {
	oneStep := easeSplashProgress(0, 100, 32*time.Millisecond)
	twoSteps := easeSplashProgress(easeSplashProgress(0, 100, 16*time.Millisecond), 100, 16*time.Millisecond)
	if math.Abs(oneStep-twoSteps) > 1e-9 {
		t.Fatalf("one step = %f, two steps = %f", oneStep, twoSteps)
	}
	if oneStep <= 0 || oneStep >= 100 {
		t.Fatalf("eased progress = %f", oneStep)
	}
}

func TestSplashMarqueeTraversesWholeBar(t *testing.T) {
	const barWidth = 300.0
	const trailWidth = 200.0
	positions := []float64{
		splashMarqueeStart(0, barWidth, trailWidth),
		splashMarqueeStart(350*time.Millisecond, barWidth, trailWidth),
		splashMarqueeStart(700*time.Millisecond, barWidth, trailWidth),
		splashMarqueeStart(1050*time.Millisecond, barWidth, trailWidth),
		splashMarqueeStart(1399*time.Millisecond, barWidth, trailWidth),
	}
	if math.Abs(positions[0]+trailWidth) > 1e-9 {
		t.Fatalf("start = %f, want %f", positions[0], -trailWidth)
	}
	for index := 1; index < len(positions); index++ {
		if positions[index] <= positions[index-1] {
			t.Fatalf("positions are not increasing: %v", positions)
		}
	}
	if positions[len(positions)-1] < barWidth-0.01 {
		t.Fatalf("end = %f, want approximately %f", positions[len(positions)-1], barWidth)
	}
}

func TestSplashCapsuleCoverage(t *testing.T) {
	if got := splashCapsuleCoverage(50, 3, 0, 0, 100, 6); got != 1 {
		t.Fatalf("center coverage = %f", got)
	}
	if got := splashCapsuleCoverage(-2, 3, 0, 0, 100, 6); got != 0 {
		t.Fatalf("outside coverage = %f", got)
	}
	if got := splashCapsuleCoverage(1, 1, 0, 0, 100, 6); got <= 0 || got >= 1 {
		t.Fatalf("edge coverage = %f", got)
	}
}

func TestLargestPNGFrameFromRealIcon(t *testing.T) {
	iconData, err := os.ReadFile("icon.ico")
	if err != nil {
		t.Fatal(err)
	}
	frame, ok := largestPNGFrame(iconData)
	if !ok {
		t.Fatal("no PNG frame found in icon.ico")
	}
	if len(frame) < 512 || string(frame[1:4]) != "PNG" {
		t.Fatalf("invalid PNG frame of %d bytes", len(frame))
	}
	logo := decodeSplashLogo(iconData, 72, 72, splashPixel(0x15, 0x17, 0x1A))
	if len(logo) != 72*72 {
		t.Fatalf("decoded logo has %d pixels", len(logo))
	}
}

func TestLargestPNGFrameRejectsJunk(t *testing.T) {
	cases := map[string][]byte{
		"empty":       {},
		"short":       {0, 0, 1},
		"not an icon": {0, 0, 2, 0, 1, 0},
		"zero count":  {0, 0, 1, 0, 0, 0},
		"bad offset":  {0, 0, 1, 0, 1, 0, 16, 16, 0, 0, 1, 0, 32, 0, 0xff, 0xff, 0xff, 0x7f, 0xff, 0xff, 0xff, 0x7f},
	}
	for name, data := range cases {
		if _, ok := largestPNGFrame(data); ok {
			t.Errorf("%s: accepted invalid icon data", name)
		}
	}
}

func TestNativeSplashLifecycle(t *testing.T) {
	if os.Getenv("GLM_SPLASH_INTEGRATION") != "1" {
		t.Skip("set GLM_SPLASH_INTEGRATION=1 to show the native splash")
	}
	directory := t.TempDir()
	startedAt := time.Now()
	s := newSplash(directory)
	select {
	case <-s.firstPaint:
	case <-time.After(2 * time.Second):
		t.Fatal("native splash did not paint")
	}
	s.mu.Lock()
	firstPaintAt := s.firstPaintAt
	shownAt := s.shownAt
	s.mu.Unlock()
	if firstPaintAt.IsZero() {
		t.Fatal("native splash initialization failed")
	}
	t.Logf("native splash: visible=%v first-pixel=%v paint=%v", shownAt.Sub(startedAt), firstPaintAt.Sub(startedAt), firstPaintAt.Sub(shownAt))
	s.set(0, "Downloading update v2.1.1...")
	time.Sleep(400 * time.Millisecond)
	s.set(58, "Downloading update v2.1.1  58%  (11.6 MB of 20.0 MB)")
	time.Sleep(700 * time.Millisecond)
	s.set(splashIndeterminate, "Installing update v2.1.1...")
	time.Sleep(700 * time.Millisecond)
	s.set(splashIndeterminate, "Starting GLM Proxy...")
	time.Sleep(400 * time.Millisecond)
	s.close()
	closedAt := time.Now()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("native splash did not stop")
	}
	s.mu.Lock()
	frames := s.frames
	s.mu.Unlock()
	t.Logf("native splash animation: %d frames, %.1f fps", frames, float64(frames)/closedAt.Sub(shownAt).Seconds())
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("native splash wrote files: %v", entries)
	}
}

func TestHumanMB(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{0, "0.0 MB"},
		{1 << 20, "1.0 MB"},
		{10 * (1 << 20), "10.0 MB"},
	}
	for _, test := range cases {
		if got := humanMB(test.input); got != test.want {
			t.Errorf("humanMB(%d) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestProgressMeterReportsPercentAndCompletion(t *testing.T) {
	type sample struct {
		percent int
		text    string
	}
	var samples []sample
	meter := &progressMeter{
		total: 100,
		label: "Downloading update v9.9.9",
		report: func(percent int, text string) {
			samples = append(samples, sample{percent: percent, text: text})
		},
	}
	for range 10 {
		if _, err := meter.Write(make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
	}
	if len(samples) == 0 {
		t.Fatal("no progress reported")
	}
	last := samples[len(samples)-1]
	if last.percent != 100 {
		t.Errorf("final percent = %d, want 100", last.percent)
	}
	if !strings.Contains(last.text, "Downloading update v9.9.9") {
		t.Errorf("label missing from %q", last.text)
	}
	if meter.written != 100 {
		t.Errorf("written = %d, want 100", meter.written)
	}
}

func TestProgressMeterUnknownTotal(t *testing.T) {
	last := 0
	meter := &progressMeter{label: "Downloading", report: func(percent int, _ string) { last = percent }}
	if _, err := meter.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if last != splashIndeterminate {
		t.Errorf("percent = %d, want %d", last, splashIndeterminate)
	}
}

func TestProgressMeterWithoutReporter(t *testing.T) {
	meter := &progressMeter{total: 10}
	if written, err := meter.Write(make([]byte, 10)); err != nil || written != 10 {
		t.Fatalf("Write = (%d, %v), want (10, nil)", written, err)
	}
}
