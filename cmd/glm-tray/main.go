//go:build windows

// glm-tray is the Windows system-tray supervisor for the GLM proxy.
//
// It launches zai-api.exe as a hidden child, restarts it if it crashes, and
// exposes three actions from the tray icon:
//
//	Monitor       open a console tailing the live proxy log
//	Update token  paste a new ZAI_TOKEN; the .env is rewritten and the proxy
//	              is restarted so it takes effect
//	Exit          stop this instance (autostart brings it back next login)
//
// The proxy writes its own rotating log file; the tray only supervises the
// process and drives that small menu, so it stays tiny and dependency-light.
package main

import (
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
)

// supervisor owns the lifecycle of the proxy child process. Each child is
// waited on exactly once, by the goroutine spawned in start; the loop only
// observes the exited channel that goroutine closes.
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
	return s, nil
}

// run is the supervision loop: (re)start the child, wait for it to exit, and
// restart unless we asked it to stop. It returns only when quit is closed.
func (s *supervisor) run() {
	if err := os.MkdirAll(filepath.Join(s.dir, "logs"), 0o755); err != nil {
		notify("GLM proxy", "could not create logs directory: "+err.Error())
	}
	backoff := time.Second
	for {
		exited := s.start()
		if exited == nil {
			// Spawn failed. Back off, then retry unless we are quitting.
			select {
			case <-s.quit:
				return
			case <-time.After(backoff):
			}
			backoff = capDur(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second // a clean spawn resets the backoff

		select {
		case <-s.quit:
			s.killChild()
			return
		case <-s.restart:
			s.killChild()
			<-exited // let the reaper finish before relaunching
		case <-exited:
			if s.stopping.Load() {
				// A stop we asked for that did not go through restart/quit;
				// wait for the next explicit trigger.
				select {
				case <-s.quit:
					return
				case <-s.restart:
				}
				continue
			}
			// Unexpected exit (crash): pause so a boot-looping proxy cannot
			// peg the CPU.
			select {
			case <-s.quit:
				return
			case <-time.After(backoff):
			}
			backoff = capDur(backoff*2, 30*time.Second)
		}
	}
}

// start launches the child with stdout+stderr redirected to the console log,
// so Monitor tails exactly what a local run prints. It returns a channel that
// is closed once the child has been reaped, or nil if the spawn failed. The
// single Wait lives in the reaper goroutine here.
func (s *supervisor) start() <-chan struct{} {
	s.stopping.Store(false)

	logFile, err := os.Create(s.consolePath)
	if err != nil {
		notify("GLM proxy", "cannot open console log: "+err.Error())
	}

	cmd := exec.Command(s.exePath)
	cmd.Dir = s.dir
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	// Fully windowless: no console flashes on screen; Monitor surfaces the
	// logs on demand instead.
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
			logFile.Close()
		}
		close(exited)
	}()
	return exited
}

// killChild terminates the current child and waits for the reaper. On Windows
// os.Process.Signal cannot deliver SIGTERM, so this is a hard kill: pooled Z.AI
// chat sessions are not cleared on a tray-driven stop (they are ephemeral, and
// a direct `zai-api.exe` run still shuts down gracefully on CTRL+C).
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

// triggerRestart stops the child; the loop then brings it back.
func (s *supervisor) triggerRestart() {
	s.stopping.Store(true)
	select {
	case s.restart <- struct{}{}:
	default:
	}
}

// shutdown stops the child and ends the supervision loop for good.
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

var sup *supervisor

func main() {
	s, err := newSupervisor()
	if err != nil {
		notify("GLM proxy", err.Error())
		os.Exit(1)
	}
	sup = s
	initJobObject()
	go s.run()
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("GLM Proxy")
	systray.SetTooltip("GLM Proxy (Z.AI) — running")

	mMonitor := systray.AddMenuItem("Monitor", "Open a window with the live proxy logs")
	mToken := systray.AddMenuItem("Update token", "Set a new ZAI token and restart the proxy")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("Exit", "Stop this proxy instance (restarts on next login)")

	go func() {
		for {
			select {
			case <-mMonitor.ClickedCh:
				openMonitor(sup.consolePath)
			case <-mToken.ClickedCh:
				handleUpdateToken()
			case <-mExit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	if sup != nil {
		sup.shutdown()
	}
}

// handleUpdateToken prompts for a new ZAI token, writes it to .env, and
// restarts the proxy so it takes effect.
func handleUpdateToken() {
	token, ok := promptToken()
	if !ok {
		return
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	envPath := filepath.Join(sup.dir, envFileName)
	if err := setEnvValue(envPath, "ZAI_TOKEN", token); err != nil {
		notify("GLM proxy", "failed to update .env: "+err.Error())
		return
	}
	notify("GLM proxy", "Token updated. Restarting proxy...")
	sup.triggerRestart()
}
