package zbridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// ============================================================================
// Z.AI SIGNATURE GENERATION
// ============================================================================

func generateZaSignature(prompt, token, userID string) (signature, timestamp, urlParams string) {
	tsMs := time.Now().UnixMilli()
	timestamp = strconv.FormatInt(tsMs, 10)
	requestId := randomUUID()
	bucket := tsMs / 300000

	mac := hmac.New(sha256.New, []byte(session.SaltKey))
	mac.Write([]byte(strconv.FormatInt(bucket, 10)))
	wKey := hex.EncodeToString(mac.Sum(nil))

	type kv struct{ k, v string }
	payloadDict := []kv{
		{"requestId", requestId},
		{"timestamp", timestamp},
		{"user_id", userID},
	}
	sort.Slice(payloadDict, func(i, j int) bool {
		return payloadDict[i].k < payloadDict[j].k
	})
	var parts []string
	for _, p := range payloadDict {
		parts = append(parts, p.k+","+p.v)
	}
	sortedPayload := strings.Join(parts, ",")

	promptB64 := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(prompt)))
	dataToSign := sortedPayload + "|" + promptB64 + "|" + timestamp

	mac2 := hmac.New(sha256.New, []byte(wKey))
	mac2.Write([]byte(dataToSign))
	signature = hex.EncodeToString(mac2.Sum(nil))

	params := url.Values{}
	params.Set("timestamp", timestamp)
	params.Set("requestId", requestId)
	params.Set("user_id", userID)
	params.Set("version", "0.0.1")
	params.Set("platform", "web")
	params.Set("token", token)
	params.Set("user_agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0.0.0")
	params.Set("language", "en-US")
	params.Set("screen_resolution", "1920x1080")
	params.Set("viewport_size", "1920x1080")
	params.Set("timezone", "Europe/Paris")
	params.Set("timezone_offset", "-60")
	params.Set("signature_timestamp", timestamp)
	urlParams = params.Encode()

	return
}

// ============================================================================
// JWT DECODE
// ============================================================================

func decodeJWT(token string) (id, name string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	decoded, err := base64Decode(parts[1])
	if err != nil {
		return "", ""
	}
	var data map[string]interface{}
	if err := json.Unmarshal(decoded, &data); err != nil {
		return "", ""
	}
	id, _ = data["id"].(string)
	email, _ := data["email"].(string)
	name = "Guest"
	if email != "" {
		name = strings.Split(email, "@")[0]
	}
	return id, name
}

// ============================================================================
// Z.AI SESSION INITIALIZATION
// ============================================================================

func scrapeConfig() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL, nil)
	if err != nil {
		logWarnf("[Config] Scrape error: %s, using default feVersion", err.Error())
		return
	}
	resp, err := zaiHTTPClient.Do(req)
	if err != nil {
		logWarnf("[Config] Scrape error: %s, using default feVersion", err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if match := feVersionRe.Find(body); match != nil {
		version := string(match)
		session.mu.Lock()
		session.FeVersion = version
		session.mu.Unlock()
		logInfof("[Config] fe_version: %s", version)
	}
}

// initializeSession establishes (or refreshes) the upstream Z.AI session.
// Concurrent callers all observe the same outcome: the first performs the
// handshake, the rest block on a broadcast channel and receive its error.
func initializeSession() error {
	session.mu.Lock()
	if wait := session.initWait; wait != nil {
		session.mu.Unlock()
		<-wait
		session.mu.Lock()
		err := session.initErr
		session.mu.Unlock()
		return err
	}
	wait := make(chan struct{})
	session.initWait = wait
	session.Initializing = true
	session.mu.Unlock()

	err := doInitializeSession()

	session.mu.Lock()
	session.initErr = err
	session.initWait = nil
	session.Initializing = false
	session.mu.Unlock()
	close(wait)

	return err
}

// doInitializeSession performs the handshake. Shared session fields are read
// concurrently by /status, DeleteZAIChat and the request path, so every write
// here holds session.mu.
func doInitializeSession() error {
	if config.ZaiToken != "" {
		logInfof("[Session] Using ZAI_TOKEN from the environment, skipping guest init.")
		id, name := decodeJWT(config.ZaiToken)

		session.mu.Lock()
		session.Token = config.ZaiToken
		session.UserID = id
		if name != "" {
			session.UserName = name
		}
		if session.UserID == "" {
			session.UserName = "User"
		}
		uidPreview := session.UserID
		session.Initialized = true
		userName := session.UserName
		session.mu.Unlock()

		if len(uidPreview) > 8 {
			uidPreview = uidPreview[:8]
		}
		logInfof("[Session] Token user: %s... (%s)", uidPreview, userName)
		return nil
	}

	logInfof("[Session] Initializing Z.AI session...")

	scrapeConfig()

	headers := map[string]string{
		"Origin":       BASE_URL,
		"Referer":      BASE_URL + "/",
		"User-Agent":   zaiUserAgent,
		"Content-Type": "application/json",
	}

	// Fire-and-forget: this primes the anti-bot cookies.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel1()
	req1, _ := http.NewRequestWithContext(ctx1, "POST", BASE_URL+"/api/v1/auths/guest", strings.NewReader("{}"))
	for k, v := range headers {
		req1.Header.Set(k, v)
	}
	zaiHTTPClient.Do(req1)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, "GET", BASE_URL+"/api/v1/auths/", nil)
	for k, v := range headers {
		req2.Header.Set(k, v)
	}
	markSessionFailed := func(err error) error {
		logErrorf("[Session] Initialization error: %s", err.Error())
		session.mu.Lock()
		session.Initialized = false
		session.mu.Unlock()
		return err
	}

	resp, err := zaiHTTPClient.Do(req2)
	if err != nil {
		return markSessionFailed(err)
	}

	if resp.StatusCode != 200 {
		resp.Body.Close()
		return markSessionFailed(fmt.Errorf("Auth failed: %d", resp.StatusCode))
	}

	var authData struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&authData)
	resp.Body.Close()
	token := authData.Token

	if token == "" {
		ctx3, cancel3 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel3()
		req3, _ := http.NewRequestWithContext(ctx3, "POST", BASE_URL+"/api/v1/auths/guest", strings.NewReader("{}"))
		for k, v := range headers {
			req3.Header.Set(k, v)
		}
		guestResp, err := zaiHTTPClient.Do(req3)
		if err == nil {
			var gd struct {
				Token string `json:"token"`
			}
			_ = json.NewDecoder(io.LimitReader(guestResp.Body, 1<<20)).Decode(&gd)
			guestResp.Body.Close()
			token = gd.Token
		}
	}

	if token == "" {
		return markSessionFailed(errors.New("No token received from Z.AI"))
	}

	id, name := decodeJWT(token)
	session.mu.Lock()
	session.Token = token
	session.UserID = id
	if name != "" {
		session.UserName = name
	}
	uidPreview := session.UserID
	userName := session.UserName
	session.Initialized = true
	session.mu.Unlock()

	if len(uidPreview) > 8 {
		uidPreview = uidPreview[:8]
	}
	logInfof("[Session] Connected. UserID: %s... (%s)", uidPreview, userName)
	return nil
}

// ============================================================================
// Z.AI COMMUNICATION
// ============================================================================

// sendToZAI starts one upstream completion and returns its result stream.
// ctx governs the whole exchange: cancelling it (client disconnect, handler
// error, request timeout) tears down the upstream request and unblocks the
// producer goroutine.
func sendToZAI(ctx context.Context, prompt string, opts SendOptions) (<-chan ZAIResult, error) {
	session.mu.Lock()
	defaultChatID := session.ChatID
	defaultMessages := session.Messages
	initialized := session.Initialized
	session.mu.Unlock()

	model := opts.Model
	if model == "" {
		model = "glm-4.7"
	}

	featuresMap := resolveFeaturesForModel(model)

	// Per-request overrides take highest precedence.
	if opts.WebSearch != nil {
		if *opts.WebSearch {
			featuresMap["auto_web_search"] = true
			featuresMap["web_search"] = true
		} else {
			delete(featuresMap, "auto_web_search")
			delete(featuresMap, "web_search")
		}
	}
	if opts.Thinking != nil {
		featuresMap["enable_thinking"] = *opts.Thinking
	}
	if opts.ImageGen != nil {
		featuresMap["image_generation"] = *opts.ImageGen
	}
	if opts.PreviewMode != nil {
		featuresMap["preview_mode"] = *opts.PreviewMode
	}

	// Models that do not support reasoning_effort malfunction if they receive
	// it, so any stale value is stripped before the opt-in check below.
	delete(featuresMap, "reasoning_effort")

	if opts.ReasoningEffort != "" {
		if modelSupportsReasoningEffort(model) {
			if isValidReasoningEffort(opts.ReasoningEffort) {
				featuresMap["reasoning_effort"] = opts.ReasoningEffort
				// reasoning_effort requires thinking; a user override of
				// enable_thinking is ignored here.
				featuresMap["enable_thinking"] = true
				logInfo(fmt.Sprintf(
					"[reasoning_effort] model=%s effort=%s enabled (enable_thinking forced true)",
					model, opts.ReasoningEffort))
			} else {
				logError(fmt.Sprintf(
					"[reasoning_effort] invalid value '%s' for model=%s (accepted: high, max); ignored",
					opts.ReasoningEffort, model))
			}
		} else {
			logInfo(fmt.Sprintf(
				"[reasoning_effort] model=%s does not support reasoning_effort; parameter ignored",
				model))
		}
	}

	// Only enable_thinking reaches the request, and image_generation is never
	// enabled on this endpoint.
	delete(featuresMap, "think")
	featuresMap["image_generation"] = false

	chatID := opts.ChatID
	if chatID == "" {
		chatID = defaultChatID
	}
	messages := opts.Messages
	if messages == nil {
		messages = defaultMessages
	}

	if !initialized {
		if err := initializeSession(); err != nil {
			return nil, err
		}
	}

	resolvedOpts := struct {
		Model, ChatID     string
		FeaturesMap       map[string]interface{}
		Messages          []Message
		ClientMessagesRaw json.RawMessage
	}{
		Model:             model,
		ChatID:            chatID,
		FeaturesMap:       featuresMap,
		Messages:          messages,
		ClientMessagesRaw: opts.ClientMessagesRaw,
	}

	ch := make(chan ZAIResult, 100)
	go func() {
		defer close(ch)
		err := sendToZAIStream(ctx, prompt, resolvedOpts, ch)
		if err != nil {
			if ctx.Err() != nil {
				return // the consumer is gone; nobody is left to read this
			}
			select {
			case ch <- ZAIResult{Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}

func sendToZAIStream(ctx context.Context, prompt string, opts struct {
	Model, ChatID     string
	FeaturesMap       map[string]interface{}
	Messages          []Message
	ClientMessagesRaw json.RawMessage
}, ch chan<- ZAIResult) error {

	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		session.mu.Lock()
		token := session.Token
		userID := session.UserID
		feVersion := session.FeVersion
		session.mu.Unlock()

		signature, _, _ := generateZaSignature(prompt, token, userID)
		urlStr := BASE_URL + "/api/v2/chat/completions"

		var messagesField interface{}
		if len(opts.ClientMessagesRaw) > 0 {
			messagesField = json.RawMessage(opts.ClientMessagesRaw)
		} else {
			forwarded := make([]Message, 0, len(opts.Messages)+1)
			forwarded = append(forwarded, opts.Messages...)
			promptJSON, _ := json.Marshal(prompt)
			forwarded = append(forwarded, Message{Role: "user", Content: json.RawMessage(promptJSON)})
			messagesField = forwarded
		}

		captchaParam, err := getCaptchaVerifyParam(ctx)
		if err != nil {
			return err
		}

		featuresPayload := make(map[string]interface{}, len(opts.FeaturesMap)+2)
		for k, v := range opts.FeaturesMap {
			featuresPayload[k] = v
		}
		delete(featuresPayload, "think")
		featuresPayload["flags"] = []interface{}{}
		featuresPayload["image_generation"] = false

		requestBody := map[string]interface{}{
			"model":                opts.Model,
			"chat_id":              opts.ChatID,
			"messages":             messagesField,
			"signature_prompt":     prompt,
			"stream":               true,
			"captcha_verify_param": captchaParam,
			"features":             featuresPayload,
		}

		bodyBytes, _ := json.Marshal(requestBody)

		if debugEnabled() {
			logDebugf("Z.AI url %s", urlStr)
			logDebugf("Z.AI request body: %s", string(bodyBytes))
			hdrMap := map[string]string{
				"authorization": "Bearer " + token,
				"content-type":  "application/json",
				"x-fe-Version":  feVersion,
				"x-region":      "overseas",
				"x-signature":   signature,
			}
			hdrJSON, _ := json.MarshalIndent(hdrMap, "", "  ")
			logDebugf("Z.AI request headers %s", string(hdrJSON))
		}

		timeout := time.Duration(config.Timeouts.Default) * time.Millisecond * 2
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, "POST", urlStr, bytes.NewReader(bodyBytes))
		if err != nil {
			cancel()
			return fmt.Errorf("Z.AI connection error: %s", err.Error())
		}
		req.Header.Set("authorization", "Bearer "+token)
		req.Header.Set("User-Agent", zaiUserAgent)
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-fe-Version", feVersion)
		req.Header.Set("x-region", "overseas")
		req.Header.Set("x-signature", signature)

		metrics.upstreamAttempts.Add(1)
		sentAt := time.Now()
		resp, err := zaiHTTPClient.Do(req)
		if err != nil {
			cancel()
			metrics.upstreamErrors.Add(1)
			return fmt.Errorf("Z.AI connection error: %s", err.Error())
		}
		metrics.observeUpstreamLatency(time.Since(sentAt))

		if debugEnabled() {
			logDebugf("Z.AI response status: %d %s", resp.StatusCode, resp.Status)
			hdrs := map[string]string{}
			for k, v := range resp.Header {
				hdrs[k] = strings.Join(v, ", ")
			}
			hdrJSON, _ := json.MarshalIndent(hdrs, "", "  ")
			logDebugf("Z.AI response headers: %s", string(hdrJSON))
		}

		if resp.StatusCode == 401 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			cancel()
			metrics.upstreamUnauth.Add(1)
			session.mu.Lock()
			session.Initialized = false
			session.mu.Unlock()
			if err := initializeSession(); err != nil {
				return err
			}
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			metrics.upstreamErrors.Add(1)
			logDebugf("Z.AI error body: %s", string(errBody))
			return fmt.Errorf("Z.AI error %d: %s", resp.StatusCode, string(errBody))
		}

		err = streamSSEResponse(reqCtx, resp.Body, ch)
		resp.Body.Close()
		cancel()
		if err != nil {
			metrics.upstreamErrors.Add(1)
		}
		return err
	}
	return errors.New("Max retries exceeded")
}

// extractZAIError inspects a parsed Z.AI SSE payload for an embedded error
// (Z.AI sometimes returns HTTP 200 with the error inside the JSON body).
// Returns the human-readable detail string, or "" if no error is present.
func extractZAIError(j map[string]interface{}) string {
	if data, ok := j["data"].(map[string]interface{}); ok {
		if detail := zaiErrorDetail(data["error"], true); detail != "" {
			return detail
		}
		// Nested variant observed in production.
		if nested, ok := data["data"].(map[string]interface{}); ok {
			if detail := zaiErrorDetail(nested["error"], true); detail != "" {
				return detail
			}
		}
	}
	// Top-level error, for shapes that are not Z.AI's own.
	return zaiErrorDetail(j["error"], false)
}

// zaiErrorDetail pulls the human-readable message out of an error object,
// optionally appending the numeric code Z.AI attaches to its own errors.
func zaiErrorDetail(v interface{}, withCode bool) string {
	errObj, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	detail, _ := errObj["detail"].(string)
	if detail == "" {
		detail, _ = errObj["message"].(string)
	}
	if detail == "" {
		return ""
	}
	if withCode {
		if code, ok := errObj["code"]; ok && code != nil {
			return fmt.Sprintf("%s (code: %v)", detail, code)
		}
	}
	return detail
}

// statusFromError maps a Z.AI/bridge error string to an HTTP status code.
func statusFromError(errMsg string) int {
	switch {
	case strings.Contains(errMsg, "401"):
		return 401
	case strings.Contains(errMsg, "403"):
		return 403
	case strings.Contains(errMsg, "429"):
		return 429
	case strings.Contains(errMsg, "400"):
		return 400
	default:
		return 500
	}
}

// utf16IndexToByteIndex converts a UTF-16 code-unit offset into a byte offset
// within s. That is JavaScript string indexing, which is what the Z.AI web
// frontend uses for edit_index. The result clamps at the end of s and never
// lands inside a multi-byte rune: an offset between the two units of a
// surrogate pair is clamped to the start of that rune.
func utf16IndexToByteIndex(s string, utf16Idx int) int {
	if utf16Idx <= 0 {
		return 0
	}
	byteIdx, units := 0, 0
	for byteIdx < len(s) {
		if units == utf16Idx {
			return byteIdx
		}
		r, size := utf8.DecodeRuneInString(s[byteIdx:])
		ru := utf16.RuneLen(r)
		if ru < 0 {
			ru = 1 // invalid byte: the JS frontend also sees one unit here
		}
		if units+ru > utf16Idx {
			return byteIdx // inside a surrogate pair — clamp to rune start
		}
		units += ru
		byteIdx += size
	}
	return len(s)
}

// utf16IndexToByteIndexBytes is utf16IndexToByteIndex over the raw accumulator,
// so the streaming path never materialises it as a string to locate an offset.
func utf16IndexToByteIndexBytes(b []byte, utf16Idx int) int {
	if utf16Idx <= 0 {
		return 0
	}
	byteIdx, units := 0, 0
	for byteIdx < len(b) {
		if units == utf16Idx {
			return byteIdx
		}
		r, size := utf8.DecodeRune(b[byteIdx:])
		ru := utf16.RuneLen(r)
		if ru < 0 {
			ru = 1 // invalid byte: the JS frontend also sees one unit here
		}
		if units+ru > utf16Idx {
			return byteIdx // inside a surrogate pair — clamp to rune start
		}
		units += ru
		byteIdx += size
	}
	return len(b)
}

// commonPrefixLen returns the byte length of the longest common prefix of
// a and b. The result is always on a rune boundary, so slicing either
// string at that offset cannot produce invalid UTF-8.
func commonPrefixLen(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) {
		ra, sa := utf8.DecodeRuneInString(a[i:])
		rb, _ := utf8.DecodeRuneInString(b[i:])
		if ra != rb {
			break
		}
		i += sa
	}
	return i
}

// holdBackTail trims up to n runes from the end of s (rune-safe).
func holdBackTail(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	i, count := len(s), 0
	for i > 0 && count < n {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
		count++
	}
	return s[:i]
}

// holdBackPartialDetailsTag trims a trailing fragment that could still be
// the beginning of a <details> tag whose completion has not arrived yet,
// so a tag streamed character by character never leaks to the client.
// A COMPLETE "</details>" literal is kept (legitimate text); a complete
// "<details" is held (waiting for its ">" to decide whether it is a tag).
func holdBackPartialDetailsTag(s string) string {
	i := strings.LastIndex(s, "<")
	if i < 0 {
		return s
	}
	suffix := s[i:]
	if len(suffix) <= len("<details") && strings.HasPrefix("<details", suffix) {
		return s[:i]
	}
	if len(suffix) < len("</details>") && strings.HasPrefix("</details>", suffix) {
		return s[:i]
	}
	return s
}

// holdBackPartialQuoteMarker trims a trailing ">" that forms the whole last
// line of s. Reasoning lines are markdown-quoted ("> ..."), so mid-stream a
// new line's marker appears as a bare ">" that stripDetailsTags cannot strip
// until the space arrives. Forwarding it makes the stripped snapshot sequence
// non-monotonic, which diverges the reasoning emitter and duplicates
// everything after that point. The final flush releases it.
func holdBackPartialQuoteMarker(s string) string {
	if !strings.HasSuffix(s, ">") {
		return s
	}
	body := s[:len(s)-1]
	if body == "" || strings.HasSuffix(body, "\n") {
		return body
	}
	return s
}

// sseEmitter forwards snapshots of a growing (and occasionally rewritten) text
// to an append-only consumer as rune-safe deltas. It never emits a slice that
// starts inside a multi-byte rune, so the consumer cannot receive invalid UTF-8
// — which its JSON renderer would show as U+FFFD garble (issue #23).
type sseEmitter struct {
	clientView string // exactly what the consumer has received so far
}

// delta returns the text to append so the consumer converges on target, and
// updates the tracked view:
//   - target extends the view: the new suffix.
//   - target is a prefix of the view (a deep edit truncated the text): nothing,
//     since an append-only consumer cannot take text back. The view is kept so
//     later growth is not re-sent from a rewound base.
//   - target rewrote part of the view: everything after the longest common
//     prefix. The stale fragment in between stays on the consumer, which is
//     unavoidable here but remains valid UTF-8. The view then re-syncs to
//     target, because keeping the stale fragment would make every later
//     snapshot diverge at the same point and re-emit the rest each time.
func (e *sseEmitter) delta(target string) string {
	if target == e.clientView {
		return ""
	}
	if strings.HasPrefix(target, e.clientView) {
		delta := target[len(e.clientView):]
		e.clientView = target
		return delta
	}
	cp := commonPrefixLen(e.clientView, target)
	if cp == len(target) {
		return "" // consumer already has everything target contains
	}
	delta := target[cp:]
	e.clientView = target
	return delta
}

// splitDetails extracts every complete <details ...>...</details> block
// from raw: the block bodies (concatenated) become reasoning, everything
// else becomes content. A trailing opener whose '>' has not arrived yet is
// held pending (neither reasoning nor content) until more data arrives.
func splitDetails(raw string) (reasoning, content string) {
	var s detailsSplitter
	return s.finish([]byte(raw))
}

const (
	detailsOpen  = "<details"
	detailsClose = "</details>"
)

var (
	detailsOpenB  = []byte(detailsOpen)
	detailsCloseB = []byte(detailsClose)
)

// detailsSplitter is the resumable form of splitDetails, used by the streaming
// path where the accumulated buffer is re-classified on every SSE event.
// Re-splitting the whole buffer each time would make the per-event cost grow
// with the response; this remembers how far it consumed and appends only the
// new bytes. Both accumulators are strings.Builders, whose String() aliases
// the existing buffer, so a steady-state event allocates nothing.
type detailsSplitter struct {
	reasoning strings.Builder
	content   strings.Builder
	consumed  int  // bytes of the raw buffer already classified
	inDetails bool // inside a <details ...> body
	tail      tailKind
}

// tailKind records why feed stopped, so finish() can release the withheld
// tail into the same bucket the one-shot splitter would have put it in.
type tailKind uint8

const (
	tailContent   tailKind = iota // withheld fragment belongs to content
	tailReasoning                 // withheld fragment belongs to reasoning
	tailDropped                   // incomplete opener: belongs to neither
)

func (s *detailsSplitter) reset() {
	s.reasoning.Reset()
	s.content.Reset()
	s.consumed = 0
	s.inDetails = false
	s.tail = tailContent
}

// feed classifies the unconsumed tail of raw and returns the full reasoning
// and content snapshots. raw must extend what was previously fed; callers call
// reset() first when the buffer was rewritten behind s.consumed.
func (s *detailsSplitter) feed(raw []byte) (reasoning, content string) {
	s.tail = tailContent
	for s.consumed < len(raw) {
		rest := raw[s.consumed:]

		if s.inDetails {
			closeIdx := bytes.Index(rest, detailsCloseB)
			if closeIdx < 0 {
				// Body still streaming. Hold back a trailing fragment that
				// could still grow into the closing tag: consuming it would
				// put "</detai" into the reasoning accumulator, which is
				// append-only and could not take it back once the tag
				// completed.
				keep := len(rest) - partialTagSuffixLen(rest, detailsCloseB)
				s.reasoning.Write(rest[:keep])
				s.consumed += keep
				s.tail = tailReasoning
				break
			}
			s.reasoning.Write(rest[:closeIdx])
			s.consumed += closeIdx + len(detailsCloseB)
			s.inDetails = false
			continue
		}

		idx := bytes.Index(rest, detailsOpenB)
		if idx < 0 {
			// Same reasoning as above for a fragment that could grow into an
			// opening tag.
			keep := len(rest) - partialTagSuffixLen(rest, detailsOpenB)
			s.content.Write(rest[:keep])
			s.consumed += keep
			s.tail = tailContent
			break
		}
		s.content.Write(rest[:idx])
		s.consumed += idx

		tagEnd := bytes.IndexByte(raw[s.consumed:], '>')
		if tagEnd < 0 {
			s.tail = tailDropped // incomplete opener at the tail
			break
		}
		s.consumed += tagEnd + 1
		s.inDetails = true
	}
	return s.reasoning.String(), s.content.String()
}

// finish classifies everything left over, including fragments that feed held
// back in case they were the start of a tag. Used for the final flush, where
// nothing more is coming and a withheld fragment is just text.
func (s *detailsSplitter) finish(raw []byte) (reasoning, content string) {
	reasoning, content = s.feed(raw)
	if s.consumed >= len(raw) {
		return reasoning, content
	}
	switch s.tail {
	case tailReasoning:
		s.reasoning.Write(raw[s.consumed:])
	case tailContent:
		s.content.Write(raw[s.consumed:])
	case tailDropped:
		// An opener that never completed is not text; the one-shot splitter
		// discards it too.
	}
	s.consumed = len(raw)
	return s.reasoning.String(), s.content.String()
}

// partialTagSuffixLen returns the length of the suffix of s that is a
// non-empty prefix of tag but not the whole tag — the fragment of a tag that
// is still arriving. A complete tag returns 0 because it needs no holding.
func partialTagSuffixLen(s, tag []byte) int {
	max := len(tag) - 1
	if max > len(s) {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if s[len(s)-n] != tag[0] {
			continue
		}
		if bytes.Equal(s[len(s)-n:], tag[:n]) {
			return n
		}
	}
	return 0
}

// stripDetailsTags removes the leading <details ...> opener, every </details>
// closer, and the leading "> " markdown-quote prefix of each line, in a single
// pass into one pre-sized builder.
func stripDetailsTags(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	openerRemoved := false
	lineStart := true
	for i := 0; i < len(s); {
		c := s[i]
		if c == '<' {
			if !openerRemoved && strings.HasPrefix(s[i:], detailsOpen) {
				if end := strings.IndexByte(s[i:], '>'); end >= 0 {
					i += end + 1
					openerRemoved = true
					continue
				}
			}
			if strings.HasPrefix(s[i:], detailsClose) {
				i += len(detailsClose)
				continue
			}
		}
		if lineStart && c == '>' && i+1 < len(s) && s[i+1] == ' ' {
			i += 2
			lineStart = false
			continue
		}
		b.WriteByte(c)
		lineStart = c == '\n'
		i++
	}
	return strings.TrimSpace(b.String())
}

// detailsStripCache memoises stripDetailsTags across SSE events. Once the
// model stops thinking the reasoning text is frozen while content keeps
// streaming, so most events can skip the work entirely. The snapshot aliases a
// strings.Builder buffer, making the equality check a pointer comparison.
type detailsStripCache struct {
	src    string
	result string
	valid  bool
}

func (c *detailsStripCache) strip(s string) string {
	if c.valid && c.src == s {
		return c.result
	}
	c.src, c.result, c.valid = s, stripDetailsTags(s), true
	return c.result
}

func streamSSEResponse(ctx context.Context, body io.Reader, ch chan<- ZAIResult) error {
	// bufio.Reader rather than bufio.Scanner: Z.AI sends full-content
	// replacements, so a single data: line grows with the answer and Scanner
	// would abort with ErrTooLong once it passed its maximum token size.
	reader := bufio.NewReaderSize(body, 64*1024)

	// A byte accumulator rather than a strings.Builder, because edit_content
	// truncates the buffer and Builder cannot truncate without discarding it.
	rawBuf := make([]byte, 0, 8*1024)
	var splitter detailsSplitter
	var stripCache detailsStripCache
	contentEmitter := &sseEmitter{}   // tracks what the client has received
	reasoningEmitter := &sseEmitter{} // same for the reasoning channel

	// send blocks until the consumer takes the result or the request is
	// cancelled, so a handler that stops reading cannot park this goroutine
	// on the channel with the upstream body still open.
	send := func(r ZAIResult) bool {
		select {
		case ch <- r:
			metrics.sseEvents.Add(1)
			return true
		case <-ctx.Done():
			return false
		}
	}

	flush := func(final bool) bool {
		// Split <details ...> ... </details> into reasoning vs content.
		var reasoning, content string
		if final {
			reasoning, content = splitter.finish(rawBuf)
		} else {
			reasoning, content = splitter.feed(rawBuf)
		}
		if reasoning != "" {
			reasoning = stripCache.strip(reasoning)
		}

		// Emit reasoning delta (prefix-aware, rune-safe). Reasoning rides
		// the same edit-based stream as content, so while the stream is
		// live it gets the same protection: hold back a small tail so
		// trailing edit_content backtracks are absorbed invisibly, hold
		// back a partial </details> close tag streamed character by
		// character (splitDetails folds it into the reasoning body until
		// it completes, so forwarding it would leak the fragment and
		// then rewind the snapshot), and hold back a partially-streamed
		// "> " quote marker that a later character would strip again
		// (non-monotonic snapshots diverge the emitter and duplicate
		// everything after them). The final flush releases everything.
		if !final {
			reasoning = holdBackTail(reasoning, config.StreamHoldback)
			reasoning = holdBackPartialDetailsTag(reasoning)
			reasoning = holdBackPartialQuoteMarker(reasoning)
		}
		if delta := reasoningEmitter.delta(reasoning); delta != "" {
			if !send(ZAIResult{Reasoning: delta}) {
				return false
			}
		}

		// While the stream is live, keep a small tail pending so ordinary
		// trailing edit_content backtracks are absorbed invisibly, and
		// never forward a fragment that could still grow into a <details>
		// tag. The final flush releases everything.
		target := content
		if !final {
			target = holdBackTail(target, config.StreamHoldback)
			target = holdBackPartialDetailsTag(target)
		}
		// FullText is the authoritative upstream snapshot, used by non-stream
		// consumers and for deep-edit re-sync detection in the handlers.
		if delta := contentEmitter.delta(target); delta != "" {
			if !send(ZAIResult{Chunk: delta, FullText: target}) {
				return false
			}
		}
		return true
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		line, readErr := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				logDebugf("Z.AI SSE line: %s", trimmed)
			}

			if strings.HasPrefix(trimmed, "data: ") {
				dataStr := trimmed[6:]
				if dataStr == "[DONE]" {
					flush(true)
					return nil
				}

				var j map[string]interface{}
				if err := json.Unmarshal([]byte(dataStr), &j); err != nil {
					logDebugf("Z.AI failed to parse SSE: %s", dataStr)
				} else {
					done, err := applySSEPayload(j, &rawBuf, &splitter)
					if err != nil {
						return err
					}
					if !flush(done) {
						return ctx.Err()
					}
					if done {
						return nil
					}
				}
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				flush(true)
				return nil
			}
			return readErr
		}
	}
}

// applySSEPayload folds one parsed upstream event into the raw accumulator and
// reports whether the stream is complete. Content semantics mirror the official
// Z.AI web frontend (prod-fe bundle):
//
//	edit_content:  content = content.substring(0, edit_index) + edit_content,
//	               where edit_index is a UTF-16 code-unit offset (JavaScript
//	               string indexing) and a missing index means full replacement
//	content:       full replacement of the accumulated text
//	delta_content: plain append
//
// A mutation that rewrites bytes the splitter already classified forces a
// rescan; otherwise the splitter resumes where it left off.
func applySSEPayload(j map[string]interface{}, rawBuf *[]byte, splitter *detailsSplitter) (done bool, err error) {
	// Z.AI sometimes returns HTTP 200 with the error inside the event body.
	if errDetail := extractZAIError(j); errDetail != "" {
		logDebugf("Z.AI inline SSE error: %s", errDetail)
		metrics.upstreamErrors.Add(1)
		return false, fmt.Errorf("Z.AI error: %s", errDetail)
	}

	data, ok := j["data"].(map[string]interface{})
	if !ok {
		return false, nil
	}
	if phase, ok := data["phase"].(string); ok && phase == "done" {
		return true, nil
	}

	buf := *rawBuf
	rewound := false

	if ec, ok := data["edit_content"].(string); ok && ec != "" {
		editIndex := 0
		if ei, ok := data["edit_index"].(float64); ok {
			editIndex = int(ei)
		}
		byteIdx := utf16IndexToByteIndexBytes(buf, editIndex)
		rewound = byteIdx < splitter.consumed
		buf = append(buf[:byteIdx], ec...)
	} else if tc, ok := data["content"].(string); ok && tc != "" {
		rewound = !stringHasBytesPrefix(tc, buf[:splitter.consumed])
		buf = append(buf[:0], tc...)
	} else if dc, ok := data["delta_content"].(string); ok && dc != "" {
		buf = append(buf, dc...)
	}

	*rawBuf = buf
	if rewound {
		splitter.reset()
	}
	return false, nil
}

// stringHasBytesPrefix reports whether s starts with p, without the allocation
// a []byte-to-string conversion would cost on this path.
func stringHasBytesPrefix(s string, p []byte) bool {
	if len(s) < len(p) {
		return false
	}
	for i := range p {
		if s[i] != p[i] {
			return false
		}
	}
	return true
}
