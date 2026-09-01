//go:build windows

// glm-tray is the Windows system-tray supervisor for the GLM proxy. It runs
// zai-api.exe as a hidden child, restarts it if it crashes, and drives this menu:
//
//	Monitor Logs       follow the live proxy log in a console window
//	Change token       rewrite ZAI_TOKEN in .env and restart the proxy
//	Restart to Update  shown only once a newer release is downloaded and
//	                   verified; installs it and relaunches
//	Exit               stop this instance (autostart returns it next login);
//	                   a staged update is installed on the way out
//
// Update polling lives in update.go.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fyne.io/systray"
)

//go:embed icon.ico
var trayIcon []byte

const (
	proxyExeName = "zai-api.exe"
	envFileName  = ".env"
	consoleLog   = "proxy-console.log"

	// Written when a child exits, so each run is visually separated in Monitor.
	consoleLogSeparator = "--------------------------------------------------------------"

	// Rotated at this size rather than truncated per start, so an open Monitor
	// window keeps its place across a proxy restart.
	consoleLogMaxBytes = 8 << 20

	// A child gone sooner than this failed at startup, not while serving.
	fastExitThreshold     = 3 * time.Second
	fastExitsBeforeNotify = 3

	monitorScriptName = "monitor.ps1"

	instanceMutexName = "GLM-Proxy-Tray-Singleton"
)

// supervisor owns the proxy child's lifecycle. Each child is waited on exactly
// once, in the goroutine start spawns; the loop only watches the exited channel.
type supervisor struct {
	dir         string // install directory (holds the exe, .env, logs/)
	exePath     string
	consolePath string

	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{} // closed when the current child has been reaped
	stopping atomic.Bool   // set when we intend the child to stop

	restart  chan struct{}
	quitOnce sync.Once
	quit     chan struct{}
}

func newSupervisor() (*supervisor, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(exe)
	s := &supervisor{
		dir:         dir,
		exePath:     filepath.Join(dir, proxyExeName),
		consolePath: filepath.Join(dir, "logs", consoleLog),
		restart:     make(chan struct{}, 1),
		quit:        make(chan struct{}),
	}
	if _, err := os.Stat(s.exePath); err != nil {
		return nil, fmt.Errorf("%s not found next to the tray app: %w", proxyExeName, err)
	}
	// Must exist before anything writes into it: the updater logs here during
	// the launch-time check, which runs before the supervision loop.
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		notify("GLM proxy", "could not create logs directory: "+err.Error())
	}
	return s, nil
}

// run is the supervision loop: start, wait, restart unless we asked it to stop.
// Returns only once quit is closed.
func (s *supervisor) run() {
	backoff := time.Second
	fastExits := 0
	for {
		startedAt := time.Now()
		exited := s.start()
		if exited == nil {
			// Spawn failed: back off, then retry unless quitting.
			select {
			case <-s.quit:
				return
			case <-time.After(backoff):
			}
			backoff = capDur(backoff*2, 30*time.Second)
			continue
		}

		select {
		case <-s.quit:
			s.killChild()
			return
		case <-s.restart:
			s.killChild()
			<-exited // let the reaper finish before relaunching
		case <-exited:
			if s.stopping.Load() {
				// A stop we asked for that bypassed restart/quit; wait for the
				// next explicit trigger.
				select {
				case <-s.quit:
					return
				case <-s.restart:
				}
				continue
			}
			// Repeatedly instant death is something restarting cannot fix, and
			// looping quietly makes a broken install look like a working one.
			if time.Since(startedAt) < fastExitThreshold {
				fastExits++
				if fastExits == fastExitsBeforeNotify {
					logUpdate("proxy exited immediately %d times; surfacing to the user", fastExits)
					notify("GLM proxy", "The proxy keeps exiting as soon as it starts, so "+
						"something is stopping it from running.\n\nRight-click the tray icon and "+
						"choose Monitor Logs to see the reason. A port already in use is the usual cause.")
				}
			} else {
				// A child that ran a while and then died is a normal crash: restart
				// promptly and forget the earlier fast deaths.
				fastExits = 0
				backoff = time.Second
			}

			// Pause so a boot-looping proxy cannot peg the CPU.
			select {
			case <-s.quit:
				return
			case <-time.After(backoff):
			}
			// Grow only while still fast-failing, so a busy port or corrupt db backs
			// off to the 30s cap instead of respawning once a second forever.
			if fastExits > 0 {
				backoff = capDur(backoff*2, 30*time.Second)
			}
		}
	}
}

// start redirects the child's stdout and stderr to the console log, so Monitor
// shows exactly what a local run prints. Returns a channel closed once the child
// is reaped, or nil if the spawn failed; the single Wait lives in the reaper.
func (s *supervisor) start() <-chan struct{} {
	s.stopping.Store(false)

	logFile := s.openConsoleLog()

	cmd := exec.Command(s.exePath)
	cmd.Dir = s.dir
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	// Windowless: no console flash on screen, Monitor surfaces logs on demand.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}

	if err := cmd.Start(); err != nil {
		notify("GLM proxy", "failed to start proxy: "+err.Error())
		if logFile != nil {
			logFile.Close()
		}
		return nil
	}

	assignToJob(cmd.Process.Pid)

	exited := make(chan struct{})
	s.mu.Lock()
	s.cmd = cmd
	s.exited = exited
	s.mu.Unlock()

	go func() {
		_ = cmd.Wait() // the one and only Wait for this child
		if logFile != nil {
			// Separator, so the next run's output is not mistaken for this one's.
			fmt.Fprintf(logFile, "\n===== proxy stopped %s =====\n%s\n",
				time.Now().Format("2006-01-02 15:04:05"), consoleLogSeparator)
			logFile.Close()
		}
		close(exited)
	}()
	return exited
}

// openConsoleLog appends to the log Monitor follows, rotating only past the size
// cap. It must not truncate: an already-open Monitor keeps its read offset, so a
// restart would leave it showing nothing until the new output passed the old
// length. Appending also preserves the evidence from a crash-restart loop.
func (s *supervisor) openConsoleLog() *os.File {
	if info, err := os.Stat(s.consolePath); err == nil && info.Size() > consoleLogMaxBytes {
		previous := s.consolePath + ".1"
		_ = os.Remove(previous)
		_ = os.Rename(s.consolePath, previous)
	}

	f, err := os.OpenFile(s.consolePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// Worth a dialog: with no destination os/exec hands the child the null
		// device for its whole life, silently discarding all proxy output.
		logUpdate("cannot open %s (%v); proxy output will not be captured", s.consolePath, err)
		notify("GLM proxy", "Cannot open the console log, so Monitor will stay empty:\n"+err.Error())
		return nil
	}
	fmt.Fprintf(f, "\n===== proxy started %s =====\n", time.Now().Format("2006-01-02 15:04:05"))
	return f
}

// killChild terminates the child and waits for the reaper. Windows has no
// SIGTERM to deliver, so this is a hard kill and pooled chat sessions are not
// cleared; they are ephemeral, and a direct run still exits cleanly on CTRL+C.
func (s *supervisor) killChild() {
	s.mu.Lock()
	cmd, exited := s.cmd, s.exited
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	s.stopping.Store(true)
	_ = cmd.Process.Kill()
	if exited != nil {
		select {
		case <-exited:
		case <-time.After(6 * time.Second):
		}
	}
}

// triggerRestart stops the child and lets the loop bring it back. It kills the
// child itself rather than leaving that to the loop: queueing the signal alone
// was lost whenever the loop sat in its restart backoff instead of the select,
// so Change token left the old token running.
func (s *supervisor) triggerRestart() {
	s.stopping.Store(true)
	select {
	case s.restart <- struct{}{}:
	default:
	}
	s.killChild()
}

// shutdown stops the child and ends the loop for good.
func (s *supervisor) shutdown() {
	s.stopping.Store(true)
	s.quitOnce.Do(func() { close(s.quit) })
	s.killChild()
}

func capDur(d, max time.Duration) time.Duration {
	if d > max {
		return max
	}
	return d
}

var (
	sup           *supervisor
	spl           *splash
	upd           *updater
	updCancel     context.CancelFunc
	mUpdate       *systray.MenuItem
	updateApplied atomic.Bool
)

func main() {
	// One tray per session: two would supervise two proxies fighting over the
	// same port, and an update relaunch must not race the old process.
	if !acquireSingleInstance(instanceMutexName) {
		return
	}

	s, err := newSupervisor()
	if err != nil {
		notify("GLM proxy", err.Error())
		os.Exit(1)
	}
	sup = s

	// One window covers the whole launch: the update check, any download and
	// install, then the wait for the proxy to answer.
	spl = newSplash(sup.dir)

	// An update staged earlier, or published while this machine was off, is
	// installed before the proxy starts; the installer then relaunches the tray.
	if applyUpdateBeforeStart(spl) {
		// Exiting takes the splash with it, and the relaunched tray shows its own.
		return
	}

	spl.set(splashIndeterminate, "Starting GLM Proxy...")
	initJobObject()
	go s.run()
	go func() {
		waitProxyReady(sup.dir, 20*time.Second)
		spl.close()
	}()
	systray.Run(onReady, onExit)
}

// applyUpdateBeforeStart reports true when an installer was launched and this
// process should exit. Update states are reported on the splash.
func applyUpdateBeforeStart(sp *splash) bool {
	if !updatable() || !autoUpdateEnabled(filepath.Join(sup.dir, envFileName)) {
		return false
	}
	u := newUpdater(sup.dir, nil)
	u.onStatus = sp.set
	return u.applyStartupUpdate(context.Background())
}

func onReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("GLM Proxy")
	systray.SetTooltip("GLM Proxy (Z.AI) " + appVersion + " — running")

	mMonitor := systray.AddMenuItem("Monitor Logs", "Open a window with the live proxy logs")
	mToken := systray.AddMenuItem("Change token", "Set a new Z.AI token and restart the proxy")
	mUpdate = systray.AddMenuItem("Restart to Update", "Install the downloaded update and restart")
	mUpdate.Hide()
	systray.AddSeparator()
	mExit := systray.AddMenuItem("Exit proxy", "Stop this proxy instance (restarts on next login)")

	startUpdater()

	go func() {
		for {
			select {
			case <-mMonitor.ClickedCh:
				openMonitor(sup.consolePath)
			case <-mToken.ClickedCh:
				handleUpdateToken()
			case <-mUpdate.ClickedCh:
				if handleRestartToUpdate() {
					return
				}
			case <-mExit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

// startUpdater begins hourly polling unless the build is unversioned or .env
// disabled it.
func startUpdater() {
	if !updatable() {
		return
	}
	if !autoUpdateEnabled(filepath.Join(sup.dir, envFileName)) {
		return
	}
	upd = newUpdater(sup.dir, onUpdateStaged)
	ctx, cancel := context.WithCancel(context.Background())
	updCancel = cancel
	go upd.run(ctx)
}

// onUpdateStaged reveals the extra menu entry once a verified installer is on
// disk. Runs on the poller goroutine.
func onUpdateStaged(version string) {
	if mUpdate == nil {
		return
	}
	mUpdate.SetTitle("Restart to Update (v" + version + ")")
	mUpdate.SetTooltip("Version " + version + " is downloaded and verified. Install it now and restart.")
	mUpdate.Show()
	systray.SetTooltip("GLM Proxy (Z.AI) " + appVersion + " — update to v" + version + " ready")
}

// handleRestartToUpdate installs the staged version and relaunches, returning true
// once shutdown has begun. The installer is launched before the proxy is torn down:
// if it fails to start, the running proxy is untouched and the tray stays usable
// instead of being left dead behind an unresponsive menu.
func handleRestartToUpdate() bool {
	version, _, ok := upd.staged()
	if !ok {
		notify("GLM proxy", "No update is staged yet.")
		return false
	}
	updateApplied.Store(true)
	if err := upd.applyNow(true); err != nil {
		updateApplied.Store(false)
		logUpdate("apply failed: %v", err)
		notify("GLM proxy", "Update to v"+version+" failed to start:\n"+err.Error()+
			"\n\nThe proxy is still running.")
		return false
	}
	if updCancel != nil {
		updCancel()
	}
	if sup != nil {
		sup.shutdown()
	}
	systray.Quit()
	return true
}

// onExit stops the proxy and installs any waiting update on the way out;
// autostart brings the new tray back next login.
func onExit() {
	if updCancel != nil {
		updCancel()
	}
	if sup != nil {
		sup.shutdown()
	}
	if upd == nil || updateApplied.Load() {
		return
	}
	if _, _, ok := upd.staged(); !ok {
		return
	}
	if err := upd.applyNow(false); err != nil {
		logUpdate("apply on exit failed: %v", err)
	}
}

// handleUpdateToken prompts for a token, writes it to .env and restarts.
func handleUpdateToken() {
	token, ok := promptToken()
	if !ok {
		return // cancelled
	}
	token = strings.TrimSpace(token)
	if token == "" {
		notify("GLM proxy", "No token entered, so nothing changed.")
		return
	}
	// Match the installer's shape check so the two entry points behave alike; a
	// Z.AI JWT always starts with eyJ, and this is the usual paste mistake.
	if !strings.HasPrefix(token, "eyJ") {
		notify("GLM proxy", "That does not look like a Z.AI token (it should start "+
			"with eyJ). Applying it anyway; if requests fail, use Change token again.")
	}
	envPath := filepath.Join(sup.dir, envFileName)
	if err := setEnvValue(envPath, "ZAI_TOKEN", token); err != nil {
		notify("GLM proxy", "failed to update .env: "+err.Error())
		return
	}
	notify("GLM proxy", "Token updated. Restarting proxy...")
	sup.triggerRestart()
}
