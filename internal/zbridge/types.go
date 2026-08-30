package zbridge

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

type Features struct {
	WebSearch     bool `json:"webSearch"`
	AutoWebSearch bool `json:"autoWebSearch"`
	Thinking      bool `json:"thinking"`
	ImageGen      bool `json:"imageGen"`
	PreviewMode   bool `json:"previewMode"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type SessionState struct {
	mu           sync.Mutex
	Token        string
	UserID       string
	UserName     string
	ChatID       string
	Messages     []Message
	SaltKey      string
	FeVersion    string
	Features     Features
	Initialized  bool
	Initializing bool
	// initWait closes when an in-flight init finishes and initErr carries the
	// outcome, so concurrent callers see the same result instead of assuming
	// success.
	initWait chan struct{}
	initErr  error
}

type ZAIResult struct {
	Chunk     string
	FullText  string
	Reasoning string
	Err       error
}

type SendOptions struct {
	Model             string
	WebSearch         *bool
	Thinking          *bool
	ImageGen          *bool
	PreviewMode       *bool
	ChatID            string
	Messages          []Message
	ClientMessagesRaw json.RawMessage
	ToolsRaw          json.RawMessage
	ReasoningEffort   string // "high" or "max"; forwarded only if the model supports it
	// SignaturePrompt is signed and sent as signature_prompt (the latest user
	// turn). Empty falls back to the flat prompt.
	SignaturePrompt string
}

type ResponseResult struct {
	Content      string
	Text         string
	Prompt       string
	FinishReason string
	Reasoning    string
}

// Aliyun captcha wire types.

type InitCaptchaResponse struct {
	CertifyID string `json:"CertifyId"`
}

type CVP struct {
	CertifyID   string `json:"certifyId"`
	Data        string `json:"data"`
	DeviceToken string `json:"deviceToken"`
	SceneID     string `json:"sceneId"`
}

type VerifyCaptchaResponse struct {
	Success bool `json:"Success"`
	Result  struct {
		VerifyResult  bool   `json:"VerifyResult"`
		SecurityToken string `json:"securityToken"`
		CertifyID     string `json:"certifyId"`
	} `json:"Result"`
}

type FinalPayload struct {
	CertifyID     string `json:"certifyId"`
	IsSign        bool   `json:"isSign"`
	SceneID       string `json:"sceneId"`
	SecurityToken string `json:"securityToken"`
}

type TrackList struct {
	FI        string `json:"fi"`
	KS        string `json:"ks"`
	MC        string `json:"mc"`
	MP        string `json:"mp"`
	MU        string `json:"mu"`
	StartTime int64  `json:"startTime"`
	TC        string `json:"tc"`
	TE        string `json:"te"`
	TMV       string `json:"tmv"`
}

type Track struct {
	TrackList      TrackList `json:"TrackList"`
	TrackStartTime int64     `json:"TrackStartTime"`
	VerifyTime     int64     `json:"VerifyTime"`
	Arg            string    `json:"arg"`
}

// Process-wide state. session is guarded by session.mu; the rest are atomics or
// written once at startup.

var (
	dbPath   string
	verbose  bool
	gRunning atomic.Bool
	globalDB *sql.DB
)

var session = &SessionState{
	ChatID:    randomUUID(),
	UserName:  "Guest",
	SaltKey:   SALT_KEY,
	FeVersion: DEFAULT_FE_VERSION,
	Features:  Features{Thinking: true}, // enable_thinking defaults on
}

type ModelInfo struct {
	ID           string
	Name         string
	Description  string
	Capabilities map[string]interface{}
}

var (
	modelsCache     []ModelInfo
	modelsCacheTime time.Time
	modelsCacheMu   sync.Mutex
	// One refresh in flight at a time; others serve the previous list.
	modelsRefreshing bool
)

const modelsCacheTTL = 5 * time.Minute

// Served when Z.AI is unreachable and the cache is empty.
var fallbackModels = []ModelInfo{
	{ID: "glm-5.3", Name: "GLM-5.3", Description: "Flagship model, excels at coding and long-horizon tasks"},
	{ID: "glm-5.2", Name: "GLM-5.2", Description: "Previous flagship model"},
	{ID: "GLM-5-Turbo", Name: "GLM-5-Turbo", Description: "New model for chat, coding, and agentic tasks"},
	{ID: "GLM-5v-Turbo", Name: "GLM-5V-Turbo", Description: "Vision model with evolved intelligence"},
	{ID: "glm-4.7", Name: "GLM-4.7", Description: "Classic high-performance model"},
}

var feVersionRe = regexp.MustCompile(`prod-fe-\d+\.\d+\.\d+`)

// ModelFeatureState is per-model feature config: IncludeAll forwards every
// server capability, Overrides are user values keyed by snake_case name.
type ModelFeatureState struct {
	IncludeAll bool
	Overrides  map[string]interface{}
}

var (
	modelFeatureStates   = make(map[string]*ModelFeatureState)
	modelFeatureStatesMu sync.Mutex
)
