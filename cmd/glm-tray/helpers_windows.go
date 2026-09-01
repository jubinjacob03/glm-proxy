//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows process-creation flags.
const (
	createNoWindow         = 0x08000000 // child runs with no console window
	detachedProcess        = 0x00000008 // installer survives this process exiting
	createBreakawayFromJob = 0x01000000 // installer escapes the kill-on-close job
)

// jobHandle binds the proxy child's lifetime to this process. KILL_ON_JOB_CLOSE
// makes the OS terminate the child and its own children (a token-collector
// browser, say) however the tray exits — clean quit, crash or Task Manager — so
// the proxy is never orphaned.
var jobHandle windows.Handle

func initJobObject() {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(h)
		return
	}
	jobHandle = h
}

// assignToJob puts a freshly started child into the kill-on-close job.
func assignToJob(pid int) {
	if jobHandle == 0 {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(jobHandle, h)
}

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOK              = 0x00000000
	mbIconInformation = 0x00000040
	mbSetForeground   = 0x00010000
	mbTopMost         = 0x00040000
)

// notify shows a small modal message box, used sparingly: startup errors and
// confirmations the user triggered. Routine status goes to the log.
func notify(title, message string) {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)
	procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(msgPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(mbOK|mbIconInformation|mbSetForeground|mbTopMost),
	)
}

// promptToken shows a single-line input box, returning false if cancelled. It
// shells out to the VisualBasic InputBox, which needs no extra dependency and no
// window of our own.
func promptToken() (string, bool) {
	const script = `Add-Type -AssemblyName Microsoft.VisualBasic; ` +
		`[Microsoft.VisualBasic.Interaction]::InputBox(` +
		`'Paste your new ZAI token. The proxy will restart to apply it.',` +
		`'GLM Proxy - Change token','')`

	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	// createNoWindow hides PowerShell's console, but NOT HideWindow: its SW_HIDE
	// propagates into the .NET InputBox, so the dialog is created invisible and,
	// being modal, Output() then blocks forever on a window nobody can see.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	token := strings.TrimRight(string(out), "\r\n")
	if token == "" {
		return "", false // cancelled or left blank
	}
	return token, true
}

// openMonitor opens a console window following the proxy log.
//
// It goes through cmd's start because that builds the console itself.
// CREATE_NEW_CONSOLE leaves cmd.Stdout nil, so os/exec hands the child the null
// device and PowerShell discards every line into a permanently empty window.
func openMonitor(logPath string) {
	// logs\ may have been deleted since startup; recreate it so os.Create below
	// does not fail with a raw path-not-found the user cannot act on.
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	if _, err := os.Stat(logPath); err != nil {
		// Give the follower something to open.
		if f, cerr := os.Create(logPath); cerr == nil {
			f.Close()
		}
	}

	// A file, not -Command: a path survives start's quoting where a script does not.
	scriptPath := filepath.Join(filepath.Dir(logPath), monitorScriptName)
	if err := os.WriteFile(scriptPath, []byte(monitorScript(logPath)), 0o644); err != nil {
		notify("GLM proxy", "Could not write the Monitor script:\n"+err.Error())
		return
	}

	cmd := exec.Command("cmd", "/c", "start", "GLM Proxy - Monitor",
		"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-NoExit", "-File", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	_ = cmd.Start()
}

// monitorScript is the follower openMonitor writes out; separate so it is testable.
func monitorScript(logPath string) string {
	quoted := "'" + strings.ReplaceAll(logPath, "'", "''") + "'"

	return strings.Join([]string{
		`$ErrorActionPreference = 'Continue'`,
		`try { $Host.UI.RawUI.WindowTitle = 'GLM Proxy - Monitor' } catch {}`,
		`try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch {}`,
		`$path = ` + quoted,
		`Write-Host ('Following ' + $path) -ForegroundColor DarkGray`,
		`Write-Host 'Closing this window stops watching only; the proxy keeps running.' -ForegroundColor DarkGray`,
		`Write-Host ''`,
		// Bounded so a long crash-restart history cannot bury the useful end.
		`$backlogMax = 262144`,
		`$pos = 0`,
		`try {`,
		`  $len = (Get-Item -LiteralPath $path).Length`,
		`  if ($len -gt $backlogMax) { $pos = $len - $backlogMax }`,
		`} catch {}`,
		`while ($true) {`,
		`  try {`,
		`    $share = [System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete`,
		`    $fs = New-Object System.IO.FileStream($path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, $share)`,
		`    if ($fs.Length -lt $pos) { $pos = 0 }`,
		`    [void]$fs.Seek($pos, [System.IO.SeekOrigin]::Begin)`,
		`    $sr = New-Object System.IO.StreamReader($fs, [System.Text.Encoding]::UTF8)`,
		`    $chunk = $sr.ReadToEnd()`,
		`    $pos = $fs.Position`,
		`    $sr.Dispose()`,
		`    $fs.Dispose()`,
		`    if ($chunk.Length -gt 0) { Write-Host -NoNewline -Object $chunk }`,
		`  } catch {`,
		`    Start-Sleep -Milliseconds 700`,
		`  }`,
		`  Start-Sleep -Milliseconds 300`,
		`}`,
	}, "\n")
}

// setEnvValue rewrites or inserts one KEY=value line, leaving the rest untouched.
// A missing file is created.
func setEnvValue(path, key, value string) error {
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(data), "\n")
	}

	newLine := key + "=" + value
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "export ")
		if eq := strings.IndexByte(trimmed, '='); eq > 0 {
			if strings.TrimSpace(trimmed[:eq]) == key {
				lines[i] = newLine
				replaced = true
				break
			}
		}
	}
	if !replaced {
		lines = append(lines, newLine)
	}

	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

// getEnvValue reads one KEY=value, returning "" if the file or key is absent.
// Surrounding quotes are stripped.
func getEnvValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 || strings.TrimSpace(trimmed[:eq]) != key {
			continue
		}
		val := strings.TrimSpace(trimmed[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		return val
	}
	return ""
}

// launchInstaller starts the staged NSIS installer silently and detached. It
// stops this tray itself, so it must outlive us: DETACHED_PROCESS plus
// CREATE_BREAKAWAY_FROM_JOB keeps it clear of our console and of the
// kill-on-close job that owns the proxy child.
//
// With relaunch set, /RESTART starts the new tray when it finishes; otherwise the
// app returns at the next login through the Run key.
func launchInstaller(installerPath string, relaunch bool) error {
	args := []string{"/S"}
	if relaunch {
		args = append(args, "/RESTART")
	}
	cmd := exec.Command(installerPath, args...)
	cmd.Dir = filepath.Dir(installerPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow | detachedProcess | createBreakawayFromJob,
	}
	if err := cmd.Start(); err != nil {
		// A job that forbids breakaway rejects the flag, so retry without it.
		cmd = exec.Command(installerPath, args...)
		cmd.Dir = filepath.Dir(installerPath)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: createNoWindow | detachedProcess,
		}
		if err2 := cmd.Start(); err2 != nil {
			return err2
		}
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return nil
}

// logUpdate appends to the tray's own update log; update activity is background
// work, so it is recorded rather than shown as a dialog.
func logUpdate(format string, args ...interface{}) {
	line := time.Now().Format("2006/01/02 15:04:05") + " " + fmt.Sprintf(format, args...) + "\r\n"
	if sup == nil {
		return
	}
	path := filepath.Join(sup.dir, "logs", "update.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutexW = kernel32.NewProc("CreateMutexW")

	// Held for the life of the process; Windows releases the mutex on exit,
	// however that exit happens.
	instanceHandle uintptr
)

const errAlreadyExists = syscall.Errno(183)

// acquireSingleInstance takes a named mutex so only one tray runs per session;
// otherwise autostart plus a manual launch would leave two trays supervising two
// proxies fighting over the same port.
//
// An update relaunch is the tricky case: the installer force-kills the old tray
// and starts the new one immediately, so the mutex may still be held briefly.
// Giving up would leave the app not running at all after an update, so this
// retries and only refuses once another instance is clearly alive.
func acquireSingleInstance(name string) bool {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return true // cannot build the name; never block startup over it
	}
	deadline := time.Now().Add(12 * time.Second)
	for {
		h, _, lastErr := procCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))
		if h == 0 {
			return true // cannot create the mutex; prefer running over refusing
		}
		if lastErr != errAlreadyExists {
			instanceHandle = h
			return true
		}
		windows.CloseHandle(windows.Handle(h))
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(400 * time.Millisecond)
	}
}
