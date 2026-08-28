package zbridge

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver registration (no CGO)
)

// ============================================================================
// DEVICE TOKEN STORE — SQLite, pure-Go driver (no CGO)
// ============================================================================

var (
	stmtClaimToken *sql.Stmt
	stmtCountToken *sql.Stmt

	// tokenCount caches SELECT COUNT(*) so /health, /metrics and the
	// replenishment monitor do not each drive an index scan.
	tokenCount     atomic.Int64
	tokenCountAtNs atomic.Int64
)

const tokenCountTTL = 2 * time.Second

// initDB opens the device-token store. The PRAGMAs mirror the collector's so
// the two processes cooperate on the same file instead of colliding on locks.
func initDB() error {
	dsn := "file:" + filepath.ToSlash(dbPath) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=cache_size(-16384)" +
		"&_pragma=temp_store(MEMORY)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	// SQLite serialises writes internally, so one connection avoids both pool
	// overhead and lock contention.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Force the connection open so the PRAGMAs take effect now.
	if err := db.Ping(); err != nil {
		db.Close()
		return err
	}

	// Create the schema if the file is new. This lets the proxy start against a
	// fresh install with no tokens yet: it comes up, the token monitor fills the
	// store in the background, and requests succeed once tokens arrive. Without
	// this the prepared statements below would fail on a missing table. The
	// collector uses the identical schema, so an existing store is unchanged.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tokens (
		id    INTEGER PRIMARY KEY AUTOINCREMENT,
		token TEXT    NOT NULL,
		batch INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return fmt.Errorf("create tokens table: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tokens_batch ON tokens(batch)`); err != nil {
		db.Close()
		return fmt.Errorf("create tokens index: %w", err)
	}

	// Claim-and-delete in one statement so two concurrent generators can never
	// be handed the same token.
	claim, err := db.Prepare(
		`DELETE FROM tokens WHERE id = (SELECT id FROM tokens ORDER BY id LIMIT 1) RETURNING token`)
	if err != nil {
		db.Close()
		return fmt.Errorf("prepare token claim: %w", err)
	}
	count, err := db.Prepare(`SELECT COUNT(*) FROM tokens`)
	if err != nil {
		claim.Close()
		db.Close()
		return fmt.Errorf("prepare token count: %w", err)
	}

	globalDB, stmtClaimToken, stmtCountToken = db, claim, count
	refreshTokenCount()
	return nil
}

// closeDB releases the prepared statements and the connection.
func closeDB() {
	if stmtClaimToken != nil {
		stmtClaimToken.Close()
		stmtClaimToken = nil
	}
	if stmtCountToken != nil {
		stmtCountToken.Close()
		stmtCountToken = nil
	}
	if globalDB != nil {
		globalDB.Close()
		globalDB = nil
	}
}

// claimToken atomically removes and returns the oldest device token. A token
// is single-use, so claiming and deleting are the same operation.
func claimToken() (string, bool) {
	if stmtClaimToken == nil {
		logError("token store is not open")
		return "", false
	}
	var token string
	if err := stmtClaimToken.QueryRow().Scan(&token); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			tokenCount.Store(0)
			tokenCountAtNs.Store(time.Now().UnixNano())
			logError("No device tokens available in table 'tokens'")
		} else {
			logError("Failed to claim device token: " + err.Error())
		}
		return "", false
	}
	if tokenCount.Add(-1) < 0 {
		tokenCount.Store(0)
	}
	metrics.tokensConsumed.Add(1)
	return token, true
}

// getTokenCount reports the device tokens left in the store, served from a
// short-TTL cache so status endpoints stay free to poll.
func getTokenCount() int {
	if last := tokenCountAtNs.Load(); last != 0 &&
		time.Now().UnixNano()-last < int64(tokenCountTTL) {
		return int(tokenCount.Load())
	}
	return refreshTokenCount()
}

func refreshTokenCount() int {
	if globalDB == nil || stmtCountToken == nil {
		return 0
	}
	var count int
	if err := stmtCountToken.QueryRow().Scan(&count); err != nil {
		logError("Failed to query token count: " + err.Error())
		return int(tokenCount.Load())
	}
	tokenCount.Store(int64(count))
	tokenCountAtNs.Store(time.Now().UnixNano())
	return count
}

// ============================================================================
// ALIYUN SIGNATURE
// ============================================================================

func generateSignature(params map[string]string, secKey string) string {
	keys := make([]string, 0, len(params)+1)
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonical strings.Builder
	canonical.Grow(512)
	for i, k := range keys {
		if i > 0 {
			canonical.WriteByte('&')
		}
		canonical.WriteString(urlEncode(k, ""))
		canonical.WriteByte('=')
		canonical.WriteString(urlEncode(params[k], ""))
	}

	stringToSign := "POST&" + urlEncode("/", "") + "&" + urlEncode(canonical.String(), "")
	signingKey := secKey + "&"
	return base64Encode(hmacSHA1([]byte(signingKey), []byte(stringToSign)))
}

func buildQueryString(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.Grow(512)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(urlEncode(k, ""))
		b.WriteByte('=')
		b.WriteString(urlEncode(params[k], ""))
	}
	return b.String()
}

// ============================================================================
// ALIYUN HTTP
// ============================================================================

// postForm submits a form-encoded body and decodes the JSON reply into dst.
// Decoding straight off the wire avoids buffering the response and copying it
// twice on the way to json.Unmarshal.
func postForm(targetURL, body string, extraHeaders map[string]string, dst interface{}) error {
	req, err := http.NewRequest("POST", targetURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.ContentLength = int64(len(body))
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := aliyunHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()

	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(dst)
}

// ============================================================================
// CAPTCHA GENERATION — PART 1: InitCaptchaV3
// ============================================================================

func initCaptcha() (string, error) {
	params := map[string]string{
		"AccessKeyId":      accessKey,
		"Action":           "InitCaptchaV3",
		"Format":           "JSON",
		"Language":         "en",
		"Mode":             "popup",
		"SceneId":          sceneID,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   generateUUID(),
		"SignatureVersion": "1.0",
		"Timestamp":        getTimestampUTC(),
		"UpLang":           "true",
		"Version":          "2023-03-05",
	}
	params["Signature"] = generateSignature(params, secretKey)

	var result InitCaptchaResponse
	if err := postForm(
		"https://no8xfe.captcha-open-southeast.aliyuncs.com/",
		buildQueryString(params), nil, &result); err != nil {
		return "", fmt.Errorf("InitCaptchaV3: %w", err)
	}
	return result.CertifyID, nil
}

// ============================================================================
// CAPTCHA GENERATION — PART 2: Generate arg (RC4-like stream cipher)
// ============================================================================

var argPermTable = [64]int{
	32, 50, 10, 51, 6, 44, 37, 16, 46, 11, 62, 19, 43, 25, 23, 30,
	60, 33, 53, 34, 7, 26, 12, 48, 5, 2, 20, 4, 61, 13, 47, 49,
	18, 29, 27, 22, 1, 17, 39, 56, 41, 38, 55, 31, 15, 58, 52, 40,
	8, 57, 45, 35, 59, 36, 42, 54, 63, 3, 24, 28, 14, 9, 0, 21,
}

const argConstant = "4xrihv8zb8tf1mfj"

func generateArg(certifyID string) string {
	encoded := urlEncode(certifyID, "")

	// URL-decode (identity for already-decoded strings, kept for faithfulness)
	o := make([]byte, 0, len(encoded))
	for i := 0; i < len(encoded); {
		if encoded[i] == '%' && i+2 < len(encoded) {
			o = append(o, fromHex(encoded[i+1])<<4|fromHex(encoded[i+2]))
			i += 3
		} else {
			o = append(o, encoded[i])
			i++
		}
	}

	// KSA
	r := argPermTable
	n := argConstant
	rlen := 64

	i, j := 0, 0
	for i < rlen {
		j = (((i + j + r[i] + r[j]) >> 1) + int(n[i%len(n)])) & (rlen - 1)
		if i != j {
			r[i], r[j] = r[j], r[i]
		}
		i++
	}

	// PRGA
	t := make([]byte, 0, len(o))
	e, a := 0, 0
	for idx := 0; idx < len(o); idx++ {
		a = ((e ^ a) + (r[e] ^ r[a])) & (rlen - 1)
		if e != a {
			r[e], r[a] = r[a], r[e]
		}
		m := int(o[idx])
		m = m + e + r[e] - a - r[a]
		m = m ^ (r[e] + r[a])
		m = m ^ r[(r[e]+r[a])&(rlen-1)]
		m = m & 255
		t = append(t, byte(m))
		e = (e + 1) & (rlen - 1)
	}
	return base64Encode(t)
}

// ============================================================================
// CAPTCHA GENERATION — PART 4: ali_hash (custom hash with 16-byte state)
// ============================================================================

func aliHash(inputStr, saltStr string) string {
	o := inputStr
	r := saltStr
	aLen := len(o)
	m := len(r)

	var e [16]int
	for i := 0; i < 16; i++ {
		e[i] = (i << 4) + (i % 16)
	}
	f := 16

	i, j := 0, 0
	for i < f {
		j = (((i + j + e[i] + e[j]) >> 1) + int(r[i%m])) & (f - 1)
		e[i], e[j] = e[j], e[i]
		i++
	}

	idx, p, q := 0, 0, 0
	for idx < aLen {
		q = ((p ^ q) + (e[p] ^ e[q])) & (f - 1)
		e[p], e[q] = e[q], e[p]
		C := int(o[idx])
		C = (C + p + q) ^ e[p] ^ e[q]
		C = C & 255
		e[p] = C
		p = (p + 1) & (f - 1)
		idx++
	}

	for step := 0; step < 2*f; step++ {
		pos := step % f
		if pos != 0 {
			e[pos] ^= e[pos-1]
		} else {
			e[0] ^= e[f-1]
		}
	}

	var result [32]byte
	for i, b := range e {
		result[i*2] = hexLower[(b>>4)&0xF]
		result[i*2+1] = hexLower[b&0xF]
	}
	return string(result[:])
}

// ============================================================================
// CAPTCHA GENERATION — PART 7: encrypt (same RC4-like cipher, different key)
// ============================================================================

const encryptKey = "3e627e1b4c63f913"

func encrypt(plaintext []byte) string {
	o := plaintext
	n := encryptKey
	r := argPermTable
	rlen := 64

	oKsa, tKsa := 0, 0
	for oKsa < rlen {
		tKsa = (((oKsa + tKsa + r[oKsa] + r[tKsa]) >> 1) + int(n[oKsa%len(n)])) & (rlen - 1)
		if oKsa != tKsa {
			r[oKsa], r[tKsa] = r[tKsa], r[oKsa]
		}
		oKsa++
	}

	t := make([]byte, 0, len(o))
	e, a := 0, 0
	for nPrga := 0; nPrga < len(o); nPrga++ {
		a = ((e ^ a) + (r[e] ^ r[a])) & (rlen - 1)
		if e != a {
			r[e], r[a] = r[a], r[e]
		}
		m := int(o[nPrga])
		m = m + e + r[e] - a - r[a]
		m = m ^ (r[e] + r[a])
		m = m ^ r[(r[e]+r[a])&(rlen-1)]
		m = m & 255
		t = append(t, byte(m))
		e = (e + 1) & (rlen - 1)
	}
	return base64Encode(t)
}

// ============================================================================
// ZLIB COMPRESS — pooled writer, pooled output buffer
// ============================================================================

func zlibCompress(data []byte) []byte {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.Grow(len(data) + len(data)/2 + 128)

	w := zlibWriterPool.Get().(*zlib.Writer)
	w.Reset(buf)
	w.Write(data)
	w.Close()
	zlibWriterPool.Put(w)

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	bufPool.Put(buf)
	return result
}

// ============================================================================
// CAPTCHA GENERATION — PART 8: VerifyCaptchaV3
// ============================================================================

func verifyCaptcha(certifyID, dataValue, deviceToken string) (string, error) {
	cvpJSON, err := jsonMarshal(CVP{
		CertifyID:   certifyID,
		Data:        dataValue,
		DeviceToken: deviceToken,
		SceneID:     sceneID,
	})
	if err != nil {
		return "", err
	}

	params := map[string]string{
		"AccessKeyId":        accessKey,
		"Action":             "VerifyCaptchaV3",
		"Format":             "JSON",
		"SignatureMethod":    "HMAC-SHA1",
		"SignatureVersion":   "1.0",
		"Timestamp":          getTimestampUTC(),
		"Version":            "2023-03-05",
		"SceneId":            sceneID,
		"CertifyId":          certifyID,
		"CaptchaVerifyParam": string(cvpJSON),
		"SignatureNonce":     generateUUID(),
	}
	params["Signature"] = generateSignature(params, secretKey)

	var respJSON VerifyCaptchaResponse
	if err := postForm(
		"https://no8xfe-verify.captcha-open-southeast.aliyuncs.com/",
		buildQueryString(params), map[string]string{"Referer": ""}, &respJSON); err != nil {
		return "", fmt.Errorf("VerifyCaptchaV3: %w", err)
	}

	if respJSON.Success && respJSON.Result.VerifyResult {
		st := respJSON.Result.SecurityToken
		ci := respJSON.Result.CertifyID
		if st != "" && ci != "" {
			fpJSON, err := jsonMarshal(FinalPayload{
				CertifyID:     ci,
				IsSign:        true,
				SceneID:       sceneID,
				SecurityToken: st,
			})
			if err != nil {
				return "", err
			}
			return base64Encode(fpJSON), nil
		}
		logError("VerifyCaptchaV3 succeeded but securityToken/certifyId empty for deviceToken=" + deviceToken)
	} else if respJSON.Success {
		logError("deviceToken failed verification (VerifyResult=false): " + deviceToken)
	} else {
		logError("VerifyCaptchaV3 request unsuccessful for deviceToken=" + deviceToken)
	}
	return "", nil
}

// ============================================================================
// CAPTCHA VERIFY PARAM
// ============================================================================

// errNoDeviceTokens signals an empty local token store: retrying cannot help,
// only the collector can.
var errNoDeviceTokens = errors.New("no device tokens remaining")

// computeFinalPayload produces one Aliyun CaptchaVerifyParam, retrying on
// transient failures with a short backoff.
func computeFinalPayload(ctx context.Context) string {
	backoff := 250 * time.Millisecond
	for attempt := 1; attempt <= maxTokenRetries; attempt++ {
		if ctx.Err() != nil {
			return ""
		}

		payload, err := tryCompute()
		switch {
		case err == nil && payload != "":
			return payload
		case errors.Is(err, errNoDeviceTokens):
			logError(fmt.Sprintf("No device tokens remaining (attempt %d/%d) — waiting on the collector",
				attempt, maxTokenRetries))
			return ""
		case err != nil:
			logError(fmt.Sprintf("Captcha attempt %d/%d failed: %v", attempt, maxTokenRetries, err))
		default:
			logError(fmt.Sprintf("Captcha attempt %d/%d produced an empty payload", attempt, maxTokenRetries))
		}

		if attempt == maxTokenRetries {
			break
		}
		// Back off instead of hammering Aliyun (and burning a token per
		// attempt) as fast as the loop can run.
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(backoff):
		}
		if backoff < 4*time.Second {
			backoff *= 2
		}
	}
	logError(fmt.Sprintf("All %d captcha attempts exhausted", maxTokenRetries))
	return ""
}

// tryCompute runs one full captcha handshake.
//
// The device token is claimed as late as possible — right before the only
// call that consumes it. Previously it was claimed first and deleted even
// when InitCaptchaV3 or the local crypto failed, so every upstream hiccup
// permanently destroyed a token that the collector then had to re-earn
// through a headless browser run.
func tryCompute() (string, error) {
	certifyID, err := initCaptcha()
	if err != nil {
		return "", fmt.Errorf("initCaptcha: %w", err)
	}

	argValue := generateArg(certifyID)
	ct := currentTimeMillis()

	track := Track{
		TrackList: TrackList{
			StartTime: ct,
		},
		TrackStartTime: ct,
		VerifyTime:     ct + 300,
		Arg:            argValue,
	}
	jsonBytes, err := jsonMarshal(track)
	if err != nil {
		return "", err
	}

	h := aliHash(string(jsonBytes), "0000")
	combined := h + string(jsonBytes)
	compressed := zlibCompress([]byte(combined))
	fb64 := base64Encode(compressed)
	finalVal := encrypt([]byte(fb64))

	// Claimed as late as possible: this is the only call that spends the
	// token, so earlier failures cost nothing.
	deviceToken, ok := claimToken()
	if !ok {
		return "", errNoDeviceTokens
	}

	payload, err := verifyCaptcha(certifyID, finalVal, deviceToken)
	if err != nil {
		return "", fmt.Errorf("verifyCaptcha: %w", err)
	}
	return payload, nil
}

// ============================================================================
// CAPTCHA CACHE — background generation so requests do not pay the handshake
// ============================================================================

type cachedCaptcha struct {
	value       string
	generatedAt time.Time
}

type CaptchaCache struct {
	mu         sync.Mutex
	params     []cachedCaptcha
	generating int
	lastActive time.Time
	wg         sync.WaitGroup
}

var captchaCache = &CaptchaCache{lastActive: time.Now()}

func (c *CaptchaCache) markActive() {
	c.mu.Lock()
	c.lastActive = time.Now()
	c.mu.Unlock()
}

// stats reports the cache depth and how many parameters are being generated.
func (c *CaptchaCache) stats() (depth, pending int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.params), c.generating
}

// sweepLocked drops expired parameters in place, reusing the backing array.
func (c *CaptchaCache) sweepLocked() {
	ttl := config.Captcha.TTL
	kept := c.params[:0]
	for _, p := range c.params {
		if time.Since(p.generatedAt) < ttl {
			kept = append(kept, p)
		}
	}
	for i := len(kept); i < len(c.params); i++ {
		c.params[i] = cachedCaptcha{}
	}
	c.params = kept
}

func (c *CaptchaCache) Get() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastActive = time.Now()
	c.sweepLocked()

	if len(c.params) > 0 {
		val := c.params[0].value
		c.params = c.params[1:]
		return val, true
	}
	return "", false
}

// Run keeps the cache stocked while the bridge is being used. On cancellation
// it waits for in-flight generations rather than abandoning them mid-handshake.
func (c *CaptchaCache) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second): // let the session initialise first
	}

	ticker := time.NewTicker(config.Captcha.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.wg.Wait()
			return
		case <-ticker.C:
		}

		if getTokenCount() == 0 {
			continue
		}

		c.mu.Lock()
		// Each cached parameter costs a device token and expires unused, so
		// restocking while nobody is calling is a slow token leak.
		if time.Since(c.lastActive) > config.Captcha.IdleWindow {
			c.mu.Unlock()
			continue
		}
		c.sweepLocked()
		needed := config.Captcha.CacheSize - len(c.params) - c.generating
		if needed > 0 {
			c.generating += needed
		}
		c.mu.Unlock()

		for i := 0; i < needed; i++ {
			c.wg.Add(1)
			go c.generate(ctx)
		}
	}
}

func (c *CaptchaCache) generate(ctx context.Context) {
	defer c.wg.Done()
	// Released via defer so a panic in the handshake cannot leak a slot;
	// enough leaked slots would starve the cache permanently.
	defer func() {
		c.mu.Lock()
		c.generating--
		c.mu.Unlock()
	}()

	startedAt := time.Now()
	payload := computeFinalPayload(ctx)
	if payload == "" {
		metrics.captchaFailed.Add(1)
		logError("[Captcha Cache] ✗ failed to generate param")
		return
	}

	metrics.captchaGenerated.Add(1)
	c.mu.Lock()
	c.params = append(c.params, cachedCaptcha{value: payload, generatedAt: time.Now()})
	depth := len(c.params)
	c.mu.Unlock()
	logDebugf("[Captcha Cache] generated param in %.1fs (cache size: %d)",
		time.Since(startedAt).Seconds(), depth)
}

// getCaptchaVerifyParam returns a verification parameter for one upstream
// request, from the cache when agent mode keeps one warm, otherwise by running
// the handshake inline.
func getCaptchaVerifyParam(ctx context.Context) (string, error) {
	captchaCache.markActive()

	if config.AgentMode {
		if val, ok := captchaCache.Get(); ok {
			metrics.captchaCacheHits.Add(1)
			logDebugf("[Captcha Cache] hit, using cached param")
			return val, nil
		}
		metrics.captchaCacheMisses.Add(1)
		logDebugf("[Captcha Cache] miss, generating synchronously")
	}

	startedAt := time.Now()
	logDebugf("[Captcha] computing CaptchaVerifyParam")

	// Derived from the caller's context so a client that walks away stops the
	// handshake rather than spending tokens on a reply nobody will read.
	genCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	type result struct {
		val string
		err error
	}
	ch := make(chan result, 1)

	go func() {
		payload := computeFinalPayload(genCtx)
		if payload == "" {
			ch <- result{"", errors.New("captcha generation returned empty payload")}
			return
		}
		ch <- result{payload, nil}
	}()

	select {
	case r := <-ch:
		elapsed := time.Since(startedAt).Seconds()
		if r.err != nil {
			logErrorf("[Captcha] ✗ error: %s", r.err.Error())
			return "", r.err
		}
		metrics.captchaGenerated.Add(1)
		logDebugf("[Captcha] got %db in %.1fs", len(r.val), elapsed)
		return r.val, nil
	case <-genCtx.Done():
		elapsed := time.Since(startedAt).Seconds()
		metrics.captchaFailed.Add(1)
		if errors.Is(context.Cause(genCtx), context.Canceled) {
			logDebugf("[Captcha] abandoned after %.1fs (client gone)", elapsed)
			return "", context.Canceled
		}
		logErrorf("[Captcha] ✗ timeout after %.1fs", elapsed)
		return "", errors.New("captcha generation timeout after 90s")
	}
}
