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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver registration (no CGO)
)

// Device token store. Each captcha verification spends exactly one token, so an
// empty store fails every completion until the collector refills it.

var (
	stmtClaimToken *sql.Stmt
	stmtCountToken *sql.Stmt

	// Caches SELECT COUNT(*) so /health, /metrics and the monitor do not each
	// drive an index scan.
	tokenCount     atomic.Int64
	tokenCountAtNs atomic.Int64
)

const tokenCountTTL = 2 * time.Second

// initDB opens the device-token store. The PRAGMAs mirror the collector's so the
// two processes share the file instead of colliding on locks.
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
	// SQLite serialises writes anyway, so one connection avoids pool overhead and
	// lock contention both.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Force it open now, so the PRAGMAs actually take effect.
	if err := db.Ping(); err != nil {
		db.Close()
		return err
	}

	// Created here so a fresh install boots with no tokens and fills in the
	// background; without it the prepared statements below fail on a missing
	// table. The collector uses the identical schema, so existing stores are
	// untouched.
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

	// Claim and delete in one statement, so two concurrent generators can never
	// receive the same token.
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

// quarantineTokenDB renames a damaged store aside, with its WAL and shared-memory
// siblings, so a fresh initDB can recreate it. The tokens it held are disposable.
func quarantineTokenDB() error {
	stamp := time.Now().Format("20060102-150405")
	moved := false
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := dbPath + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, dbPath+".corrupt-"+stamp+suffix); err != nil {
			return err
		}
		moved = true
	}
	if !moved {
		return fmt.Errorf("no database file at %s to quarantine", dbPath)
	}
	return nil
}

// closeDB releases the prepared statements, then the connection.
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

// claimToken removes and returns the oldest token. Tokens are single-use, so
// claiming and deleting are one operation.
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
			// Not an error here: an empty store is normal on a fresh install and
			// the caller reports it with the context needed to act. As [ERROR] it
			// made first-run priming look like a fault.
			logDebugf("device token store is empty")
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

// getTokenCount reports tokens left, from a short-TTL cache so status endpoints
// stay free to poll.
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

// generateSignature is the HMAC-SHA1 Aliyun expects, over key-sorted parameters.
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
		canonical.WriteString(urlEncode(k))
		canonical.WriteByte('=')
		canonical.WriteString(urlEncode(params[k]))
	}

	stringToSign := "POST&" + urlEncode("/") + "&" + urlEncode(canonical.String())
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
		b.WriteString(urlEncode(k))
		b.WriteByte('=')
		b.WriteString(urlEncode(params[k]))
	}
	return b.String()
}

// postForm posts a form body and decodes the JSON reply into dst. Decoding off the
// wire avoids buffering the response and copying it twice.
func postForm(ctx context.Context, targetURL, body string, extraHeaders map[string]string, dst interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, strings.NewReader(body))
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

// The captcha handshake, in the order tryCompute runs it: initCaptcha to get a
// certifyID, generateArg and aliHash over the fake interaction track, encrypt,
// then verifyCaptcha to trade it all for a verification parameter.

// initCaptcha opens a handshake and returns its certifyID.
func initCaptcha(ctx context.Context) (string, error) {
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
		ctx,
		"https://no8xfe.captcha-open-southeast.aliyuncs.com/",
		buildQueryString(params), nil, &result); err != nil {
		return "", fmt.Errorf("InitCaptchaV3: %w", err)
	}
	return result.CertifyID, nil
}

// generateArg derives arg with an RC4-like cipher keyed by certifyID, permuted
// through argPermTable.

var argPermTable = [64]int{
	32, 50, 10, 51, 6, 44, 37, 16, 46, 11, 62, 19, 43, 25, 23, 30,
	60, 33, 53, 34, 7, 26, 12, 48, 5, 2, 20, 4, 61, 13, 47, 49,
	18, 29, 27, 22, 1, 17, 39, 56, 41, 38, 55, 31, 15, 58, 52, 40,
	8, 57, 45, 35, 59, 36, 42, 54, 63, 3, 24, 28, 14, 9, 0, 21,
}

const argConstant = "4xrihv8zb8tf1mfj"

func generateArg(certifyID string) string {
	o := certifyID

	// Key-scheduling: permute the state from the key.
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

	// Keystream generation, XORed into the output.
	t := make([]byte, len(o))
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
		t[idx] = byte(m)
		e = (e + 1) & (rlen - 1)
	}
	return base64Encode(t)
}

// aliHash is Aliyun's custom hash over a 16-byte state. Reverse-engineered from
// the Fielin VM bundle; see .assets/reports for the derivation.
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

// encrypt applies the same RC4-like cipher as generateArg under a fixed key.

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

// zlibCompress uses a pooled writer and output buffer: this runs once per
// captcha, which is once per request in the worst case.
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

// verifyCaptcha spends the device token and returns the verification parameter.
// This is the only call that consumes a token.
func verifyCaptcha(ctx context.Context, certifyID, dataValue, deviceToken string) (string, error) {
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
		ctx,
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

// errNoDeviceTokens signals an empty local token store: retrying cannot help,
// only the collector can.
var errNoDeviceTokens = errors.New("no device tokens remaining")

// tokenDemand lets a request that hit an empty store ask the monitor to collect
// now rather than waiting out its poll interval.
var tokenDemand = make(chan struct{}, 1)

// collectorHealthy reports whether the monitor is running and its last run
// succeeded. Waiting on a working collector is worth it; waiting on a broken one
// would stall every request for the full captcha budget, so that case fails fast.
var collectorHealthy atomic.Bool

// requestTokenCollection nudges the monitor. Non-blocking: one pending request is
// enough, since a run collects a whole batch.
func requestTokenCollection() {
	select {
	case tokenDemand <- struct{}{}:
	default:
	}
}

// waitForDeviceTokens blocks until the collector stocks the store or ctx runs
// out. A fresh install starts empty, and collection takes about half a minute
// once the browser is cached, so waiting turns what used to be a guaranteed
// failure into a merely slow first request.
func waitForDeviceTokens(ctx context.Context) bool {
	if getTokenCount() > 0 {
		return true
	}
	if !collectorHealthy.Load() {
		// Nothing will refill it: the monitor is off, or its collector is failing.
		return false
	}
	requestTokenCollection()
	logConsolef("[Tokens] store is empty; holding this request while the collector stocks it " +
		"(a first run downloads a browser too, so allow a minute or two)")

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if getTokenCount() > 0 {
				logConsolef("[Tokens] tokens available; resuming the held request")
				return true
			}
			if !collectorHealthy.Load() {
				logConsolef("[Tokens] collector is not succeeding; releasing the held request")
				return false
			}
		}
	}
}

// computeFinalPayload produces one CaptchaVerifyParam, retrying transient
// failures with a short backoff.
func computeFinalPayload(ctx context.Context) string {
	backoff := 250 * time.Millisecond
	for attempt := 1; attempt <= maxTokenRetries; attempt++ {
		if ctx.Err() != nil {
			return ""
		}

		payload, err := tryCompute(ctx)
		switch {
		case err == nil && payload != "":
			return payload
		case errors.Is(err, errNoDeviceTokens):
			// Only the collector can fix this, and it usually needs a moment, so
			// wait and retry rather than failing the caller.
			if !waitForDeviceTokens(ctx) {
				logError(fmt.Sprintf("No device tokens arrived within the request budget (attempt %d/%d)",
					attempt, maxTokenRetries))
				return ""
			}
			continue
		case err != nil:
			logError(fmt.Sprintf("Captcha attempt %d/%d failed: %v", attempt, maxTokenRetries, err))
		default:
			logError(fmt.Sprintf("Captcha attempt %d/%d produced an empty payload", attempt, maxTokenRetries))
		}

		if attempt == maxTokenRetries {
			break
		}
		// Back off rather than hammering Aliyun, and burning a token per attempt,
		// as fast as the loop can run.
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

// tryCompute runs one full captcha handshake. The device token is claimed as
// late as possible, right before the only call that spends it: claiming first
// meant every upstream hiccup destroyed a token the collector had to re-earn
// through a headless browser run.
func tryCompute(ctx context.Context) (string, error) {
	certifyID, err := initCaptcha(ctx)
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

	if err := ctx.Err(); err != nil {
		return "", err
	}
	deviceToken, ok := claimToken()
	if !ok {
		return "", errNoDeviceTokens
	}

	payload, err := verifyCaptcha(ctx, certifyID, finalVal, deviceToken)
	if err != nil {
		return "", fmt.Errorf("verifyCaptcha: %w", err)
	}
	return payload, nil
}

// Background cache, so an agent-mode request does not pay for the handshake
// inline. It pauses while idle rather than spending tokens on nobody.

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

// stats reports cache depth and how many parameters are in flight.
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

// Run keeps the cache stocked while the bridge is in use, and on cancellation
// waits for in-flight generations rather than abandoning them mid-handshake.
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
		// Each parameter costs a token and expires unused, so restocking while
		// nobody is calling is a slow token leak.
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
	// Deferred so a panic cannot leak a slot; enough leaks starve the cache.
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

// captchaUnavailableError names the actual cause instead of the internal symptom
// "empty payload". An unstocked token store is by far the most common reason and
// the only one the user can act on.
func captchaUnavailableError() error {
	if getTokenCount() == 0 {
		return errors.New("device token store is empty and the collector has not stocked it yet; " +
			"a fresh install downloads a browser first, so give it a minute and retry — " +
			"the tray Monitor window shows [Tokens] and [Collector] progress")
	}
	return errors.New("could not generate captcha verification; the proxy log records the failing step")
}

// getCaptchaVerifyParam returns one parameter, from the cache when agent mode
// keeps one warm, otherwise by running the handshake inline.
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

	// From the caller's context, so a client that walks away stops the handshake
	// instead of spending tokens on a reply nobody reads.
	genCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	type result struct {
		val string
		err error
	}
	ch := make(chan result, 1)

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				logPanic("captcha payload", rec)
				select {
				case ch <- result{"", captchaUnavailableError()}:
				default:
				}
			}
		}()
		payload := computeFinalPayload(genCtx)
		if payload == "" {
			ch <- result{"", captchaUnavailableError()}
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
