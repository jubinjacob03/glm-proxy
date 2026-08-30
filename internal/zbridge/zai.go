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
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// generateZaSignature signs a prompt the way the Z.AI web client does: the
// signing key is derived from the salt and a 5-minute time bucket, so a
// signature only stays valid inside its bucket.
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

// decodeJWT reads the id and a display name out of a token payload without
// verifying it; the upstream server is the only thing that trusts this token.
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

// scrapeConfig picks the frontend version out of the Z.AI home page. Failure is
// not fatal: the default feVersion still works.
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

// initializeSession establishes or refreshes the upstream session. Concurrent
// callers share one handshake: the first runs it, the rest block on a broadcast
// channel and get its result.
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

// doInitializeSession performs the handshake. /status, DeleteZAIChat and the
// request path all read session fields concurrently, so every write holds
// session.mu.
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

	// Only the cookies matter, but the body must still be closed: leaving it open
	// holds a connection out of the pool, and this reruns on every upstream 401.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel1()
	req1, err := http.NewRequestWithContext(ctx1, "POST", BASE_URL+"/api/v1/auths/guest", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req1.Header.Set(k, v)
	}
	if resp1, err := zaiHTTPClient.Do(req1); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp1.Body, 1<<16))
		resp1.Body.Close()
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	req2, err := http.NewRequestWithContext(ctx2, "GET", BASE_URL+"/api/v1/auths/", nil)
	if err != nil {
		return err
	}
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
		return markSessionFailed(fmt.Errorf("auth failed: %d", resp.StatusCode))
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
		return markSessionFailed(errors.New("no token received from Z.AI"))
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

// sendToZAI starts one upstream completion and returns its result stream.
// Cancelling ctx (client disconnect, handler error, timeout) tears down the
// upstream request and unblocks the producer goroutine.
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

	// Per-request overrides win over everything resolved above.
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

	// Models without reasoning_effort support malfunction if they receive it,
	// so strip any stale value before the opt-in check below.
	delete(featuresMap, "reasoning_effort")

	if opts.ReasoningEffort != "" {
		if modelSupportsReasoningEffort(model) {
			if isValidReasoningEffort(opts.ReasoningEffort) {
				featuresMap["reasoning_effort"] = opts.ReasoningEffort
				// Requires thinking, so a user override is ignored here.
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

	// Only enable_thinking reaches the wire, and image_generation never does.
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
		SignaturePrompt   string
	}{
		Model:             model,
		ChatID:            chatID,
		FeaturesMap:       featuresMap,
		Messages:          messages,
		ClientMessagesRaw: opts.ClientMessagesRaw,
		SignaturePrompt:   opts.SignaturePrompt,
	}

	ch := make(chan ZAIResult, 100)
	go func() {
		defer close(ch)
		defer func() {
			if rec := recover(); rec != nil {
				logPanic("zai producer", rec)
				select {
				case ch <- ZAIResult{Err: errors.New("internal error: upstream producer failed")}:
				case <-ctx.Done():
				}
			}
		}()
		err := sendToZAIStream(ctx, prompt, resolvedOpts, ch)
		if err != nil {
			if ctx.Err() != nil {
				return
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
	SignaturePrompt   string
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

		// Sign and send the latest user turn; the two must cover the same bytes.
		sigContent := opts.SignaturePrompt
		if sigContent == "" {
			sigContent = prompt
		}

		signature, _, _ := generateZaSignature(sigContent, token, userID)
		urlStr := BASE_URL + "/api/v2/chat/completions"

		var messagesField interface{}
		if len(opts.ClientMessagesRaw) > 0 {
			messagesField = json.RawMessage(processImagesForZAI(ctx, []byte(opts.ClientMessagesRaw), token))
		} else {
			rawMsgs, _ := json.Marshal(opts.Messages)
			var moddedMsgs []Message
			_ = json.Unmarshal(processImagesForZAI(ctx, rawMsgs, token), &moddedMsgs)

			forwarded := make([]Message, 0, len(moddedMsgs)+1)
			forwarded = append(forwarded, moddedMsgs...)
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
			"signature_prompt":     sigContent,
			"stream":               true,
			"captcha_verify_param": captchaParam,
			"features":             featuresPayload,
		}

		bodyBytes, _ := json.Marshal(requestBody)

		if debugEnabled() {
			logDebugf("Z.AI url %s", urlStr)
			logDebugf("Z.AI request body: %s", string(bodyBytes))
			hdrMap := map[string]string{
				// Never the real bearer: with ZAI_TOKEN set this is the account
				// credential, and debug logs get pasted into issues.
				"authorization": "Bearer " + redactSecret(token),
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
	return errors.New("max retries exceeded")
}

// extractZAIError returns an embedded error detail, or "" if there is none.
// Z.AI sometimes answers HTTP 200 with the error inside the JSON body.
func extractZAIError(j map[string]interface{}) string {
	if data, ok := j["data"].(map[string]interface{}); ok {
		if detail := zaiErrorDetail(data["error"], true); detail != "" {
			return detail
		}
		// A nested variant seen in production.
		if nested, ok := data["data"].(map[string]interface{}); ok {
			if detail := zaiErrorDetail(nested["error"], true); detail != "" {
				return detail
			}
		}
	}
	// Top-level, for shapes that are not Z.AI's own.
	return zaiErrorDetail(j["error"], false)
}

const (
	// Each image is uploaded separately, so the count bounds request duration
	// as much as memory.
	maxUploadImageBytes = 24 << 20
	maxImagesPerRequest = 10
)

// normalizeImageMIME collapses a MIME type to a supported value, so no client string
// reaches a header verbatim.
func normalizeImageMIME(mime string) string {
	// A remote content-type may carry parameters ("image/jpeg; charset=utf-8"),
	// which would otherwise fall through and be relabelled as PNG.
	if semi := strings.IndexByte(mime, ';'); semi >= 0 {
		mime = mime[:semi]
	}
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return "image/jpeg"
	case "image/webp":
		return "image/webp"
	case "image/gif":
		return "image/gif"
	case "image/bmp":
		return "image/bmp"
	default:
		return "image/png"
	}
}

func imageExtForMIME(mime string) string {
	switch mime {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	default:
		return ".png"
	}
}

func bodySnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// uploadImageToZAI turns an inline image (data URI, bare base64 or remote URL)
// into a Z.AI-hosted file, returning its id and signed CDN URL. The completions
// endpoint rejects inline base64, so images must be uploaded and referenced.
func uploadImageToZAI(ctx context.Context, imgData, token string) (fileID, cdnURL string, err error) {
	var fileData []byte
	mimeType := "image/png"

	switch {
	case strings.HasPrefix(imgData, "http://"), strings.HasPrefix(imgData, "https://"):
		req, rerr := http.NewRequestWithContext(ctx, "GET", imgData, nil)
		if rerr != nil {
			return "", "", rerr
		}
		if rerr := validateFetchTarget(req.URL); rerr != nil {
			return "", "", rerr
		}
		resp, rerr := imageFetchClient.Do(req)
		if rerr != nil {
			return "", "", rerr
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", "", fmt.Errorf("fetch image: HTTP %d", resp.StatusCode)
		}
		fileData, rerr = io.ReadAll(io.LimitReader(resp.Body, maxUploadImageBytes))
		if rerr != nil {
			return "", "", rerr
		}
		if ct := resp.Header.Get("content-type"); strings.HasPrefix(ct, "image/") {
			mimeType = ct
		}

	case strings.HasPrefix(imgData, "data:"):
		comma := strings.Index(imgData, ",")
		if comma == -1 {
			return "", "", fmt.Errorf("invalid data URI")
		}
		if semi := strings.Index(imgData[:comma], ";"); semi > len("data:") {
			mimeType = imgData[len("data:"):semi]
		}
		fileData, err = base64.StdEncoding.DecodeString(imgData[comma+1:])
		if err != nil {
			return "", "", fmt.Errorf("decode data URI: %w", err)
		}

	default:
		fileData, err = base64.StdEncoding.DecodeString(imgData)
		if err != nil {
			return "", "", fmt.Errorf("decode base64 image: %w", err)
		}
	}

	if len(fileData) == 0 {
		return "", "", fmt.Errorf("image payload is empty")
	}
	// The remote branch caps as it reads; the decode branches can only be checked
	// after the fact, so the same limit lands on all three.
	if int64(len(fileData)) > maxUploadImageBytes {
		return "", "", fmt.Errorf("image is %d bytes, over the %d byte limit",
			len(fileData), int64(maxUploadImageBytes))
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	hdr := make(textproto.MIMEHeader)
	// Normalised, not passed through: multipart does not sanitise header values, so a
	// CR/LF from the client's data URI would inject headers into the upstream body.
	mimeType = normalizeImageMIME(mimeType)
	hdr.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, "image"+imageExtForMIME(mimeType)))
	hdr.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(hdr)
	if err != nil {
		return "", "", err
	}
	if _, err = part.Write(fileData); err != nil {
		return "", "", err
	}
	if err = writer.WriteField("purpose", "vision"); err != nil {
		return "", "", err
	}
	if err = writer.Close(); err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", BASE_URL+"/api/v1/files/", body)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("content-type", writer.FormDataContentType())
	req.Header.Set("user-agent", zaiUserAgent)
	req.Header.Set("origin", BASE_URL)
	req.Header.Set("referer", BASE_URL+"/")

	resp, err := zaiHTTPClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", "", fmt.Errorf("upload rejected: HTTP %d: %s", resp.StatusCode, bodySnippet(raw))
	}

	var resData struct {
		ID   string `json:"id"`
		Meta struct {
			CDNURL string `json:"cdn_url"`
		} `json:"meta"`
	}
	if err = json.Unmarshal(raw, &resData); err != nil {
		return "", "", fmt.Errorf("upload response parse: %w (body: %s)", err, bodySnippet(raw))
	}
	if resData.ID == "" {
		return "", "", fmt.Errorf("upload returned no file id (body: %s)", bodySnippet(raw))
	}

	logInfof("[Image] uploaded %d bytes (%s) as %s", len(fileData), mimeType, resData.ID)
	return resData.ID, resData.Meta.CDNURL, nil
}

// processImagesForZAI replaces inline image parts with Z.AI-hosted image_url
// references. Images already served over https pass through untouched.
func processImagesForZAI(ctx context.Context, messagesRaw []byte, token string) []byte {
	var msgs []map[string]interface{}
	if err := json.Unmarshal(messagesRaw, &msgs); err != nil {
		return messagesRaw
	}
	changed := false

	for i, msg := range msgs {
		content, ok := msg["content"].([]interface{})
		if !ok {
			continue
		}
		newContent := make([]interface{}, 0, len(content))
		msgChanged := false
		for _, partRaw := range content {
			part, ok := partRaw.(map[string]interface{})
			if !ok {
				newContent = append(newContent, partRaw)
				continue
			}
			partType, _ := part["type"].(string)
			var urlStr string
			switch partType {
			case "image_url":
				if imgURLObj, ok := part["image_url"].(map[string]interface{}); ok {
					urlStr, _ = imgURLObj["url"].(string)
				}
			case "input_image":
				if src, ok := part["source"].(map[string]interface{}); ok {
					if mediaType, _ := src["media_type"].(string); mediaType != "" {
						if data, _ := src["data"].(string); data != "" {
							urlStr = "data:" + mediaType + ";base64," + data
						}
					}
				}
				if urlStr == "" {
					urlStr, _ = part["url"].(string)
				}
			default:
				newContent = append(newContent, part)
				continue
			}
			if urlStr == "" {
				newContent = append(newContent, part)
				continue
			}
			if strings.HasPrefix(urlStr, "https://") && !strings.Contains(urlStr, "base64") {
				newContent = append(newContent, map[string]interface{}{
					"type":      "image_url",
					"image_url": map[string]string{"url": urlStr},
				})
				continue
			}

			id, cdn, err := uploadImageToZAI(ctx, urlStr, token)
			if err != nil || id == "" {
				// Carries the client's URL and an upstream body snippet.
				logErrorf("[Image] upload failed: %s", printableASCII(fmt.Sprint(err)))
				newContent = append(newContent, map[string]interface{}{
					"type": "text",
					"text": "[Image could not be processed]",
				})
				msgChanged = true
				continue
			}
			ref := cdn
			if ref == "" {
				ref = BASE_URL + "/api/v1/files/" + id + "/content"
			}
			newContent = append(newContent, map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]string{"url": ref},
			})
			msgChanged = true
		}
		if msgChanged {
			changed = true
			if len(newContent) == 1 {
				if textPart, ok := newContent[0].(map[string]interface{}); ok {
					if textPart["type"] == "text" {
						msgs[i]["content"] = textPart["text"]
						continue
					}
				}
			}
			msgs[i]["content"] = newContent
		}
	}

	if !changed {
		return messagesRaw
	}
	newRaw, err := json.Marshal(msgs)
	if err != nil {
		return messagesRaw
	}
	return newRaw
}

// zaiErrorDetail pulls the readable message out of an error object, optionally
// appending the numeric code Z.AI attaches to its own.
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

// statusFromError maps an error string onto an HTTP status.
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

// utf16IndexToByteIndex converts a UTF-16 code-unit offset to a byte offset.
// That is JavaScript string indexing, which is what Z.AI's frontend uses for
// edit_index. The result clamps to the end of s and to rune starts, so an offset
// landing between the halves of a surrogate pair cannot split it.
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

// utf16IndexToByteIndexBytes is the same over the raw accumulator, so the stream
// path never materialises a string just to find an offset.
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

// commonPrefixLen returns the byte length of the longest common prefix of a and
// b, always on a rune boundary so slicing either there stays valid UTF-8.
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

// holdBackTail trims up to n runes off the end of s, on rune boundaries.
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

// holdBackPartialDetailsTag trims a trailing fragment that could still grow
// into a <details> tag, so one streamed character by character never leaks.
// A complete "</details>" is kept as legitimate text; a complete "<details" is
// held until its ">" arrives and settles whether it is a tag.
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

// holdBackPartialQuoteMarker trims a trailing ">" that forms the whole last line
// of s. Reasoning lines are markdown-quoted, so mid-stream a new line's marker
// arrives as a bare ">" that stripDetailsTags cannot strip until its space
// follows. Forwarding it makes the stripped snapshots non-monotonic, diverging
// the reasoning emitter and duplicating everything after that point. The final
// flush releases it.
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

// sseEmitter forwards snapshots of a growing, occasionally rewritten text to an
// append-only consumer as rune-safe deltas. It never starts a slice inside a
// multi-byte rune, which would reach the client as U+FFFD garble (issue #23).
type sseEmitter struct {
	clientView string // exactly what the consumer has received so far
}

// delta returns the text to append for the consumer to converge on target:
//   - target extends the view: the new suffix.
//   - target is a prefix of the view (a deep edit truncated it): nothing, since
//     an append-only consumer cannot take text back. The view is kept so later
//     growth is not re-sent from a rewound base.
//   - target rewrote part of the view: everything after the common prefix. The
//     stale fragment in between is unavoidable but stays valid UTF-8, and the
//     view re-syncs to target — keeping it would make every later snapshot
//     diverge at the same point and re-emit the rest each time.
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

// splitDetails sorts raw into reasoning (the concatenated bodies of complete
// <details ...>...</details> blocks) and content (everything else).
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

// detailsSplitter is the resumable form of splitDetails for the streaming path.
// Re-splitting the whole buffer on every SSE event would make the per-event cost
// grow with the response, so this remembers how far it consumed and appends only
// new bytes. Builder.String() aliases its buffer, so a steady-state event
// allocates nothing.
type detailsSplitter struct {
	reasoning strings.Builder
	content   strings.Builder
	consumed  int  // bytes of the raw buffer already classified
	inDetails bool // inside a <details ...> body
	tail      tailKind
}

// tailKind records why feed stopped, so finish releases the withheld tail into
// the bucket the one-shot splitter would have chosen.
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

// feed classifies the unconsumed tail and returns both full snapshots. raw must
// extend what was fed before; call reset first if the buffer was rewritten behind
// s.consumed.
func (s *detailsSplitter) feed(raw []byte) (reasoning, content string) {
	s.tail = tailContent
	for s.consumed < len(raw) {
		rest := raw[s.consumed:]

		if s.inDetails {
			closeIdx := bytes.Index(rest, detailsCloseB)
			if closeIdx < 0 {
				// Still streaming. Hold back a fragment that could grow into
				// the closing tag: the reasoning accumulator is append-only, so
				// a premature "</detai" could never be taken back.
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
			// Same again, for a fragment that could grow into an opening tag.
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

// finish is the final flush: nothing more is coming, so fragments feed held back
// as possible tag starts are just text and get released.
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
		// An opener that never completed is not text; the one-shot splitter drops
		// it too.
	}
	s.consumed = len(raw)
	return s.reasoning.String(), s.content.String()
}

// partialTagSuffixLen returns how many trailing bytes of s form an incomplete
// prefix of tag, i.e. a tag still arriving. A complete tag returns 0.
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
// closer and each line's "> " quote prefix, in one pass into a pre-sized builder.
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

// detailsStripCache memoises stripDetailsTags across SSE events. Once the model
// stops thinking, reasoning is frozen while content keeps streaming, so most
// events skip the work. The snapshot aliases a Builder buffer, so the equality
// check is effectively a pointer comparison.
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
	// Reader, not Scanner: Z.AI sends full-content replacements, so one data:
	// line grows with the answer and Scanner would abort with ErrTooLong.
	reader := bufio.NewReaderSize(body, 64*1024)

	// A byte slice, not a Builder: edit_content truncates, and Builder cannot
	// truncate without discarding everything.
	rawBuf := make([]byte, 0, 8*1024)
	var splitter detailsSplitter
	var stripCache detailsStripCache
	contentEmitter := &sseEmitter{}   // tracks what the client has received
	reasoningEmitter := &sseEmitter{} // same for the reasoning channel

	// send waits for the consumer or for cancellation, so a handler that stops
	// reading cannot park this goroutine with the upstream body still open.
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
		var reasoning, content string
		if final {
			reasoning, content = splitter.finish(rawBuf)
		} else {
			reasoning, content = splitter.feed(rawBuf)
		}
		if reasoning != "" {
			reasoning = stripCache.strip(reasoning)
		}

		// Reasoning rides the same edit-based stream as content, so it needs the
		// same three guards while the stream is live: absorb trailing
		// edit_content backtracks, and never forward a partial </details> tag or
		// quote marker, either of which would rewind a later snapshot and
		// diverge the emitter. The final flush releases everything.
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

		// Same for content, minus the quote marker: that only appears inside a
		// reasoning body.
		target := content
		if !final {
			target = holdBackTail(target, config.StreamHoldback)
			target = holdBackPartialDetailsTag(target)
		}
		// FullText is the authoritative snapshot: non-stream consumers and the
		// handlers' deep-edit re-sync detection both read it.
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

// applySSEPayload folds one event into the accumulator and reports whether the
// stream is done. The mutation shapes mirror Z.AI's own frontend (prod-fe):
//
//	edit_content:  content = content.substring(0, edit_index) + edit_content,
//	               where edit_index is a UTF-16 code-unit offset (JavaScript
//	               string indexing) and a missing index means full replacement
//	content:       full replacement of the accumulated text
//	delta_content: plain append
//
// Rewriting classified bytes forces a rescan; otherwise the splitter resumes.
func applySSEPayload(j map[string]interface{}, rawBuf *[]byte, splitter *detailsSplitter) (done bool, err error) {
	// HTTP 200 with the error inside the event body is a real upstream shape.
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

// stringHasBytesPrefix reports whether s starts with p, without the allocation a
// []byte-to-string conversion would cost here.
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
