//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows process-creation flags.
const (
	createNoWindow   = 0x08000000 // child runs with no console window
	createNewConsole = 0x00000010 // Monitor opens its own console window
)

// jobHandle binds the proxy child's lifetime to this tray process. A job with
// KILL_ON_JOB_CLOSE means the OS terminates the child (and its own children,
// e.g. a token-collector browser) whenever the tray exits for ANY reason —
// clean quit, crash, or a force-kill from Task Manager — so the proxy is never
// left orphaned.
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

// assignToJob places a freshly started child into the kill-on-close job.
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

// notify shows a small modal message box. It is used sparingly: startup errors
// and confirmations the user explicitly triggered. Routine status goes to the
// log, not the screen.
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

// promptToken shows a single-line input box and returns what the user typed.
// The second result is false if they cancelled. It shells out to the
// VisualBasic InputBox, which needs no extra dependency and no window of our
// own; the helper process itself runs without a console.
func promptToken() (string, bool) {
	const script = `Add-Type -AssemblyName Microsoft.VisualBasic; ` +
		`[Microsoft.VisualBasic.Interaction]::InputBox(` +
		`'Paste your new ZAI token. The proxy will restart to apply it.',` +
		`'GLM Proxy - Update token','')`

	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
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

// openMonitor opens a new console window tailing the proxy's console log, which
// is exactly what a local `zai-api.exe` run prints. Get-Content -Wait follows
// new lines as they arrive.
func openMonitor(logPath string) {
	if _, err := os.Stat(logPath); err != nil {
		// Create an empty file so the tail has something to follow.
		if f, cerr := os.Create(logPath); cerr == nil {
			f.Close()
		}
	}
	escaped := strings.ReplaceAll(logPath, "'", "''")
	script := "$host.UI.RawUI.WindowTitle='GLM Proxy - Monitor'; " +
		"Get-Content -LiteralPath '" + escaped + "' -Wait -Tail 200"

	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-NoExit", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
	_ = cmd.Start()
}

// setEnvValue rewrites (or inserts) a KEY=value line in an .env file, leaving
// every other line untouched. A missing file is created.
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
