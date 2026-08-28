package zbridge

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// CONFIGURATION
// ============================================================================

const (
	// Aliyun captcha credentials
	accessKey       = "LTAI5tSEBwYMwVKAQGpxmvTd"
	secretKey       = "YSKfst7GaVkXwZYvVihJsKF9r89koz"
	sceneID         = "didk33e0"
	maxTokenRetries = 5

	// Z.AI direct config
	SALT_KEY           = "key-@@@@)))()((9))-xxxx&&&%%%%%"
	DEFAULT_FE_VERSION = "prod-fe-1.1.88"
	zaiUserAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " + "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// BASE_URL is a var (not const) only so tests can point the bridge at a
// mock upstream; the default value is the production endpoint.
var BASE_URL = "https://chat.z.ai"

type Config struct {
	Server struct {
		Port int
		Host string
	}
	Auth struct {
		Enabled bool
		Token   string
	}
	Timeouts struct {
		Default int
	}
	ZaiToken  string
	AgentMode bool
	// AgentModeVariant picks the compatibility shim: "modern" (XML-sectioned
	// prompt, see agent.go) or "legacy" ([ROLE: ...] rewrite).
	AgentModeVariant string
	Logging          struct {
		Level  string // LOG_LEVEL: debug, info, warn, error, off
		Format string // LOG_FORMAT
		Dir    string // LOG_DIR, default "logs"
		// ConsoleWidth truncates console lines; the file keeps them in full.
		ConsoleWidth int // LOG_CONSOLE_WIDTH, default 150
		// Retention bounds. Rotation is size-triggered only; lumberjack has
		// no timer, so there is no time-based rotation.
		MaxSizeMB   int // LOG_MAX_SIZE_MB, default 10
		MaxBackups  int // LOG_MAX_BACKUPS, default 7
		MaxAgeDays  int // LOG_MAX_AGE_DAYS, default 7
		FlushMillis int // LOG_FLUSH_MS, default 500
	}
	MaxRequestBytes int64 // MAX_REQUEST_BYTES, default 32 MiB
	// UpstreamMaxConns bounds concurrent connections to Z.AI. The uTLS dialer
	// advertises HTTP/1.1 only, since HTTP/2 would change the fingerprint, so
	// one connection carries one in-flight stream and this is the real ceiling
	// on concurrent completions.
	UpstreamMaxConns int // UPSTREAM_MAX_CONNS, default 128
	// Captcha tunes the parameter cache. Every cached parameter costs one
	// device token and expires unused, so depth and idle window set the idle
	// token burn.
	Captcha struct {
		CacheSize    int           // CAPTCHA_CACHE_SIZE, default 2
		TTL          time.Duration // CAPTCHA_TTL_SECONDS, default 75
		IdleWindow   time.Duration // CAPTCHA_IDLE_SECONDS, default 120
		PollInterval time.Duration
	}
	TokenMonitor struct {
		Enabled       bool          // TOKEN_MONITOR, default true
		MinTokens     int           // TOKEN_MIN, default 50
		Interval      time.Duration // TOKEN_MONITOR_INTERVAL_SECONDS, default 60
		Batch         int           // TOKEN_COLLECT_BATCH, default 2
		CollectorPath string        // TOKEN_COLLECTOR_PATH, default auto-detected
		RunTimeout    time.Duration // TOKEN_COLLECT_TIMEOUT_SECONDS, default 900
	}
	// StreamHoldback is how many runes are kept pending at the tail of the
	// streamed content. Z.AI's stream is edit-based (edit_content can rewrite
	// the tail) and an append-only SSE client cannot take back text it already
	// received, so a small window lets ordinary backtracks be absorbed
	// invisibly. 0 disables it. See issue #23.
	StreamHoldback int
	// SyncMode creates a chat session per request instead of drawing from the
	// pool. Sessions are throwaway either way — see session_pool.go.
	SyncMode bool
	// SessionPoolSize is the standing batch of pre-made ready chat sessions.
	SessionPoolSize int // SESSION_POOL_SIZE, default 5
	// SessionAcquireTimeout bounds, in seconds, how long a request waits for a
	// pooled session before creating one directly. 0 waits indefinitely.
	SessionAcquireTimeout int // SESSION_ACQUIRE_TIMEOUT, default 10
}

// envInt overwrites dst from the named environment variable when it holds an
// integer of at least min.
func envInt(key string, dst *int, min int) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	if n, err := strconv.Atoi(v); err == nil && n >= min {
		*dst = n
	}
}

// envSeconds overwrites dst from the named environment variable, interpreting
// the value as a whole number of seconds of at least minSeconds.
func envSeconds(key string, dst *time.Duration, minSeconds int) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	if n, err := strconv.Atoi(v); err == nil && n >= minSeconds {
		*dst = time.Duration(n) * time.Second
	}
}

func envBool(v string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

// loadDotEnv reads simple KEY=VALUE lines from an .env file and exports each
// key that is not already set in the real environment, so an explicit env var
// or `-e` flag always wins over the file. Missing files are ignored.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 1 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" || val == "" {
			continue
		}
		if _, ok := os.LookupEnv(key); !ok {
			os.Setenv(key, val)
		}
	}
}

// loadDotEnvFiles seeds the environment from .env, checked both in the working
// directory and next to the executable. This is what lets the packaged exe run
// without the PowerShell wrapper: double-clicked, tray-launched, or as a
// service, it finds its own configuration. The working directory is preferred
// so an operator can override the baked file by launching elsewhere.
func loadDotEnvFiles() {
	// First writer wins (see loadDotEnv), so the preferred source is loaded
	// first: the working directory, then the executable's own directory.
	if wd, err := os.Getwd(); err == nil {
		loadDotEnv(filepath.Join(wd, ".env"))
	}
	if exe, err := os.Executable(); err == nil {
		loadDotEnv(filepath.Join(filepath.Dir(exe), ".env"))
	}
}

func loadConfig() *Config {
	loadDotEnvFiles()

	c := &Config{}
	c.Server.Port = 3007
	c.Server.Host = "0.0.0.0"
	c.Auth.Enabled = true
	c.Auth.Token = "Jubin"
	c.Timeouts.Default = 300000
	c.ZaiToken = ""
	c.AgentMode = false
	c.AgentModeVariant = "modern"
	// Not debug: at that level every upstream SSE line plus full request and
	// response bodies are logged, putting a mutex, a file write and a console
	// syscall on the hottest path in the process.
	c.Logging.Level = "info"
	c.Logging.Format = "text"
	c.Logging.Dir = "logs"
	c.Logging.ConsoleWidth = 150
	c.Logging.MaxSizeMB = 10
	c.Logging.MaxBackups = 7
	c.Logging.MaxAgeDays = 7
	c.Logging.FlushMillis = 500
	c.MaxRequestBytes = 32 << 20
	c.UpstreamMaxConns = 128
	c.Captcha.CacheSize = 2
	c.Captcha.TTL = 75 * time.Second
	c.Captcha.IdleWindow = 120 * time.Second
	c.Captcha.PollInterval = 500 * time.Millisecond
	c.TokenMonitor.Enabled = true
	c.TokenMonitor.MinTokens = 50
	c.TokenMonitor.Interval = 60 * time.Second
	c.TokenMonitor.Batch = 2
	c.TokenMonitor.RunTimeout = 15 * time.Minute
	c.StreamHoldback = 24
	c.SyncMode = false
	c.SessionPoolSize = defaultPoolSize
	c.SessionAcquireTimeout = int(defaultPoolWait / time.Second)

	if p := os.Getenv("PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			c.Server.Port = n
		}
	}
	if h := os.Getenv("HOST"); h != "" {
		c.Server.Host = h
	}
	if t := os.Getenv("AUTH_TOKEN"); t != "" {
		c.Auth.Token = t
	}
	if t := os.Getenv("TIMEOUT"); t != "" {
		if n, err := strconv.Atoi(t); err == nil {
			c.Timeouts.Default = n
		}
	}
	if t := os.Getenv("ZAI_TOKEN"); t != "" {
		c.ZaiToken = t
	}
	if am := os.Getenv("AGENT_MODE"); am != "" {
		switch strings.ToLower(am) {
		case "1", "true", "yes", "on", "modern":
			c.AgentMode = true
		case "legacy":
			// Explicit opt-in to the old [ROLE: ...] rewrite shim.
			c.AgentMode = true
			c.AgentModeVariant = "legacy"
		case "0", "false", "no", "off":
			c.AgentMode = false
		}
	}
	// Overrides the shim independently of the AGENT_MODE on/off switch.
	if v := os.Getenv("AGENT_MODE_VARIANT"); v != "" {
		switch strings.ToLower(v) {
		case "legacy":
			c.AgentModeVariant = "legacy"
		case "modern":
			c.AgentModeVariant = "modern"
		}
	}
	if l := os.Getenv("LOG_LEVEL"); l != "" {
		c.Logging.Level = l
	}
	if f := os.Getenv("LOG_FORMAT"); f != "" {
		c.Logging.Format = f
	}
	if d := os.Getenv("LOG_DIR"); d != "" {
		c.Logging.Dir = d
	}
	envInt("LOG_CONSOLE_WIDTH", &c.Logging.ConsoleWidth, 16)
	envInt("LOG_MAX_SIZE_MB", &c.Logging.MaxSizeMB, 1)
	envInt("LOG_MAX_BACKUPS", &c.Logging.MaxBackups, 0)
	envInt("LOG_MAX_AGE_DAYS", &c.Logging.MaxAgeDays, 0)
	envInt("LOG_FLUSH_MS", &c.Logging.FlushMillis, 1)
	if v := os.Getenv("MAX_REQUEST_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.MaxRequestBytes = n
		}
	}
	envInt("UPSTREAM_MAX_CONNS", &c.UpstreamMaxConns, 1)
	envInt("CAPTCHA_CACHE_SIZE", &c.Captcha.CacheSize, 1)
	envSeconds("CAPTCHA_TTL_SECONDS", &c.Captcha.TTL, 5)
	envSeconds("CAPTCHA_IDLE_SECONDS", &c.Captcha.IdleWindow, 5)
	if v := os.Getenv("TOKEN_MONITOR"); v != "" {
		c.TokenMonitor.Enabled = envBool(v, c.TokenMonitor.Enabled)
	}
	envInt("TOKEN_MIN", &c.TokenMonitor.MinTokens, 0)
	envInt("TOKEN_COLLECT_BATCH", &c.TokenMonitor.Batch, 1)
	envSeconds("TOKEN_MONITOR_INTERVAL_SECONDS", &c.TokenMonitor.Interval, 5)
	envSeconds("TOKEN_COLLECT_TIMEOUT_SECONDS", &c.TokenMonitor.RunTimeout, 30)
	if p := os.Getenv("TOKEN_COLLECTOR_PATH"); p != "" {
		c.TokenMonitor.CollectorPath = p
	}
	if h := os.Getenv("STREAM_HOLDBACK"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n >= 0 {
			c.StreamHoldback = n
		}
	}
	if sm := os.Getenv("SYNC_MODE"); sm != "" {
		c.SyncMode = envBool(sm, c.SyncMode)
	}
	if ps := os.Getenv("SESSION_POOL_SIZE"); ps != "" {
		if n, err := strconv.Atoi(ps); err == nil && n >= 1 {
			c.SessionPoolSize = n
		}
	}
	if at := os.Getenv("SESSION_ACQUIRE_TIMEOUT"); at != "" {
		if n, err := strconv.Atoi(at); err == nil && n >= 0 {
			c.SessionAcquireTimeout = n
		}
	}
	return c
}

var config = func() *Config {
	c := loadConfig()
	setLogLevel(c.Logging.Level)
	return c
}()

// agentModern reports whether the modern shim (agent.go) is active.
func (c *Config) agentModern() bool {
	return c.AgentMode && !strings.EqualFold(c.AgentModeVariant, "legacy")
}

// agentLegacy reports whether the legacy shim (agent_legacy.go) is active.
func (c *Config) agentLegacy() bool {
	return c.AgentMode && strings.EqualFold(c.AgentModeVariant, "legacy")
}
