//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSetEnvValueReplacesInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	os.WriteFile(path, []byte("AUTH_TOKEN=Jubin\nZAI_TOKEN=old.jwt.value\nPORT=3007\n"), 0o600)

	if err := setEnvValue(path, "ZAI_TOKEN", "new.jwt.value"); err != nil {
		t.Fatalf("setEnvValue: %v", err)
	}
	got, _ := os.ReadFile(path)
	text := string(got)

	if !strings.Contains(text, "ZAI_TOKEN=new.jwt.value") {
		t.Errorf("token not replaced:\n%s", text)
	}
	if strings.Contains(text, "old.jwt.value") {
		t.Errorf("old token still present:\n%s", text)
	}
	// Unrelated lines must survive the rewrite.
	if !strings.Contains(text, "AUTH_TOKEN=Jubin") || !strings.Contains(text, "PORT=3007") {
		t.Errorf("unrelated lines lost:\n%s", text)
	}
	// Rewritten in place, not appended: exactly one ZAI_TOKEN line.
	if n := strings.Count(text, "ZAI_TOKEN="); n != 1 {
		t.Errorf("expected 1 ZAI_TOKEN line, got %d:\n%s", n, text)
	}
}

func TestSetEnvValueInsertsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	os.WriteFile(path, []byte("AUTH_TOKEN=Jubin\n"), 0o600)

	if err := setEnvValue(path, "ZAI_TOKEN", "fresh"); err != nil {
		t.Fatalf("setEnvValue: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "ZAI_TOKEN=fresh") {
		t.Errorf("token not appended:\n%s", got)
	}
}

func TestSetEnvValueCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	if err := setEnvValue(path, "ZAI_TOKEN", "created"); err != nil {
		t.Fatalf("setEnvValue: %v", err)
	}
	got, _ := os.ReadFile(path)
	if strings.TrimSpace(string(got)) != "ZAI_TOKEN=created" {
		t.Errorf("unexpected new file content: %q", got)
	}
}

// TestSupervisorLifecycle exercises the real spawn/reap/kill path — the code that
// once double-waited — against a short-lived child, with no tray UI.
func TestSupervisorLifecycle(t *testing.T) {
	dir := t.TempDir()
	// A child that stays alive until killed: ping loops for about 30s.
	child := filepath.Join(dir, "zai-api.exe")
	if err := copyFile(os.Getenv("ComSpec"), child); err != nil {
		t.Skipf("cannot stage a stub child: %v", err)
	}

	s := &supervisor{
		dir:         dir,
		exePath:     child,
		consolePath: filepath.Join(dir, "console.log"),
		restart:     make(chan struct{}, 1),
		quit:        make(chan struct{}),
	}

	exited := s.start()
	if exited == nil {
		t.Fatal("start returned nil (spawn failed)")
	}
	s.mu.Lock()
	running := s.cmd != nil && s.cmd.Process != nil
	s.mu.Unlock()
	if !running {
		t.Fatal("no child process recorded after start")
	}

	// cmd.exe with no args and redirected stdout exits by itself; either way
	// killChild must return and the reaper must close exited exactly once.
	s.killChild()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("exited channel never closed after killChild (double-wait or leak)")
	}
}

func copyFile(src, dst string) error {
	if src == "" {
		src = `C:\Windows\System32\cmd.exe`
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}
