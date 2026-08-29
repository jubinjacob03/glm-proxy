// Exported seams for the blackbox tests in tests/: just enough live state to
// point the bridge at a mock upstream and skip the captcha machinery.

package zbridge

import "time"

// GetConfig returns the live config; callers must restore whatever they change.
func GetConfig() *Config { return config }

// OverrideSessionState swaps the session identity so tests can skip guest auth,
// returning a restore func.
func OverrideSessionState(token, userID string, initialized bool) func() {
	session.mu.Lock()
	oldToken, oldUser, oldInit := session.Token, session.UserID, session.Initialized
	session.Token, session.UserID, session.Initialized = token, userID, initialized
	session.mu.Unlock()
	return func() {
		session.mu.Lock()
		session.Token, session.UserID, session.Initialized = oldToken, oldUser, oldInit
		session.mu.Unlock()
	}
}

// DrainSessionGC waits for background session deletes to finish. A completion
// spawns a detached goroutine that reads BASE_URL; tests must drain it before
// restoring BASE_URL, or the restore races that read.
func DrainSessionGC() { waitForSessionGC(sessionGCDrain) }

// SeedCaptchaParam pushes a ready-made param into the agent-mode cache so
// requests skip the Aliyun handshake. Tests only; live, captchaCache.Run fills it.
func SeedCaptchaParam(value string) {
	captchaCache.mu.Lock()
	captchaCache.params = append(captchaCache.params, cachedCaptcha{
		value:       value,
		generatedAt: time.Now(),
	})
	captchaCache.mu.Unlock()
}
