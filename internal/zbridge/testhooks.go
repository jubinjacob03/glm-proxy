// Exported seams for the blackbox integration tests in tests/, and for
// operational scripting. They expose just enough live state to point the bridge
// at a mock upstream and bypass the captcha machinery.

package zbridge

import "time"

// GetConfig returns the live configuration. Callers may toggle fields and
// should restore whatever they change.
func GetConfig() *Config { return config }

// OverrideSessionState swaps the session identity and returns a function that
// restores the previous one, so tests can skip guest auth.
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

// SeedCaptchaParam pushes a ready-made captcha_verify_param into the agent-mode
// cache, so requests bypass the Aliyun handshake. Tests only; the live cache is
// fed by captchaCache.Run.
func SeedCaptchaParam(value string) {
	captchaCache.mu.Lock()
	captchaCache.params = append(captchaCache.params, cachedCaptcha{
		value:       value,
		generatedAt: time.Now(),
	})
	captchaCache.mu.Unlock()
}
