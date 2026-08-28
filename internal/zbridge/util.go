package zbridge

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

func init() {
	// Safe-character table for the custom URL encoder below.
	for i := 0; i < 256; i++ {
		c := byte(i)
		if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			baseSafeTable[i] = true
		}
	}
}

// Message-only shorthands over the levelled logger in logger.go.
func logError(msg string) { logErrorf("%s", msg) }
func logInfo(msg string)  { logInfof("%s", msg) }

// ============================================================================
// BUFFER POOLS
// ============================================================================

var bufPool = sync.Pool{
	New: func() interface{} { return bytes.NewBuffer(make([]byte, 0, 4096)) },
}

var zlibWriterPool = sync.Pool{
	New: func() interface{} {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.DefaultCompression)
		return w
	},
}

// ============================================================================
// HTTP CLIENTS
// ============================================================================

// Aliyun captcha API.
var aliyunHTTPClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ForceAttemptHTTP2:     true,
	},
	Timeout: 30 * time.Second,
}

// ============================================================================
// TLS FINGERPRINT SPOOFING — uTLS with a Chrome ClientHello
// ============================================================================
//
// Aliyun's ESA WAF fingerprints JA3 and blocks Go's default TLS stack.

// Shared: a net.Dialer holds no per-connection state.
var utlsDialer = &net.Dialer{
	Timeout:   15 * time.Second,
	KeepAlive: 30 * time.Second,
}

// Resolved once rather than re-reading six environment variables per dial.
var proxyForUpstream = sync.OnceValue(func() *url.URL {
	for _, key := range []string{
		"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY",
		"https_proxy", "http_proxy", "all_proxy",
	} {
		raw := os.Getenv(key)
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u
		}
	}
	return nil
})

// dialUTLS opens a TLS connection carrying a Chrome ClientHello, tunnelling
// through HTTP(S)_PROXY when one is configured.
func dialUTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	var rawConn net.Conn
	if proxyURL := proxyForUpstream(); proxyURL != nil {
		rawConn, err = dialViaProxy(ctx, proxyURL, addr)
	} else {
		rawConn, err = utlsDialer.DialContext(ctx, network, addr)
	}
	if err != nil {
		return nil, err
	}

	// HTTP/1.1 only: negotiating HTTP/2 would change the fingerprint.
	uConn := utls.UClient(rawConn, &utls.Config{
		ServerName:         host,
		NextProtos:         []string{"http/1.1"},
		InsecureSkipVerify: false,
	}, utls.HelloChrome_120)

	if err := uConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	return uConn, nil
}

// dialViaProxy establishes an HTTP CONNECT tunnel to addr through proxyURL.
func dialViaProxy(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	proxyConn, err := utlsDialer.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, fmt.Errorf("proxy connect: %w", err)
	}

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
	if _, err := proxyConn.Write([]byte(connectReq)); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("proxy CONNECT write: %w", err)
	}

	br := bufio.NewReader(proxyConn)
	line, err := br.ReadString('\n')
	if err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("proxy CONNECT read: %w", err)
	}
	if !strings.Contains(line, "200") {
		proxyConn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s", strings.TrimSpace(line))
	}
	for {
		line, err = br.ReadString('\n')
		if err != nil || strings.TrimSpace(line) == "" {
			break
		}
	}

	logDebugf("[uTLS] Using proxy %s for %s", proxyURL.Host, addr)

	// The reader may have consumed bytes belonging to the TLS handshake.
	if br.Buffered() > 0 {
		buffered := make([]byte, br.Buffered())
		br.Read(buffered)
		return &concatConn{Conn: proxyConn, buffer: buffered}, nil
	}
	return proxyConn, nil
}

// concatConn replays bytes a bufio.Reader consumed ahead of the caller.
type concatConn struct {
	net.Conn
	buffer []byte
}

func (c *concatConn) Read(b []byte) (int, error) {
	if len(c.buffer) > 0 {
		n := copy(b, c.buffer)
		c.buffer = c.buffer[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// Z.AI client with cookie jar + uTLS Chrome fingerprint
var zaiJar = &cookieJar{}

// Z.AI upstream. Because the dialer negotiates HTTP/1.1 only, one connection
// carries one in-flight stream, which makes MaxConnsPerHost the real ceiling on
// concurrent completions — past it requests block inside the transport with no
// error to report.
//
// No client-level Timeout: responses are streamed and each request carries its
// own deadline via context. ResponseHeaderTimeout bounds the part that should
// be fast, so a stalled upstream fails quickly instead of holding a session for
// the full request timeout.
var zaiHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialTLSContext:        dialUTLS,
		MaxIdleConns:          config.UpstreamMaxConns,
		MaxIdleConnsPerHost:   config.UpstreamMaxConns,
		MaxConnsPerHost:       config.UpstreamMaxConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     false,
		WriteBufferSize:       32 * 1024,
		ReadBufferSize:        64 * 1024,
	},
	Jar: zaiJar,
}

// ============================================================================
// COOKIE JAR
// ============================================================================

type cookieEntry struct {
	name   string
	value  string
	domain string
	path   string
}

type cookieJar struct {
	mu      sync.Mutex
	cookies []cookieEntry
}

func (j *cookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cookies {
		filtered := j.cookies[:0]
		for _, e := range j.cookies {
			if e.name == c.Name && e.domain == c.Domain && e.path == c.Path {
				continue
			}
			filtered = append(filtered, e)
		}
		j.cookies = filtered
		j.cookies = append(j.cookies, cookieEntry{
			name:   c.Name,
			value:  c.Value,
			domain: c.Domain,
			path:   c.Path,
		})
	}
}

func (j *cookieJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.cookies) == 0 {
		return nil
	}
	// One backing array rather than growing a pointer slice per request.
	out := make([]*http.Cookie, len(j.cookies))
	storage := make([]http.Cookie, len(j.cookies))
	for i, e := range j.cookies {
		storage[i] = http.Cookie{
			Name:   e.name,
			Value:  e.value,
			Domain: e.domain,
			Path:   e.path,
		}
		out[i] = &storage[i]
	}
	return out
}

// ============================================================================
// UTILITY FUNCTIONS
// ============================================================================

func randomUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// generateUUID builds a v4 UUID with manual hex encoding, no fmt.Sprintf.
func generateUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80

	var dst [36]byte
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			dst[j] = '-'
			j++
		}
		dst[j] = hexLower[b[i]>>4]
		dst[j+1] = hexLower[b[i]&0xF]
		j += 2
	}
	return string(dst[:])
}

func getTimestampUTC() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

func currentTimeMillis() int64 { return time.Now().UnixMilli() }

func nowUnix() int64 { return time.Now().Unix() }

// estimateTokens approximates a token count at four bytes per token.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

// getMessageContent flattens an OpenAI content field, which may be a plain
// string or an array of typed parts.
func getMessageContent(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var arr []interface{}
	if err := json.Unmarshal(content, &arr); err == nil {
		var texts []string
		for _, item := range arr {
			switch v := item.(type) {
			case string:
				texts = append(texts, v)
			case map[string]interface{}:
				t, _ := v["type"].(string)
				if t == "text" {
					if txt, ok := v["text"].(string); ok {
						texts = append(texts, txt)
					}
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(content)
}

func messagesToPrompt(messages []Message) string {
	var sb strings.Builder
	for _, msg := range messages {
		content := getMessageContent(msg.Content)
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// ============================================================================
// URL ENCODING — lookup table, no per-character allocation
// ============================================================================

const hexUpper = "0123456789ABCDEF"
const hexLower = "0123456789abcdef"

var baseSafeTable [256]bool

func urlEncode(s string, safe string) string {
	safeTable := baseSafeTable
	for i := 0; i < len(safe); i++ {
		safeTable[safe[i]] = true
	}

	var b strings.Builder
	b.Grow(len(s)*3 + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if safeTable[c] {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0x0F])
		}
	}
	return b.String()
}

func fromHex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return 0
	}
}

// ============================================================================
// CRYPTO HELPERS
// ============================================================================

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func hmacSHA1(key, msg []byte) []byte {
	h := hmac.New(sha1.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

func base64Decode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s + "=="); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s + "=="); err == nil {
		return b, nil
	}
	return nil, errors.New("base64 decode failed")
}

// jsonMarshal encodes v without HTML escaping, through a pooled buffer. The
// trailing newline json.Encoder appends is trimmed off.
func jsonMarshal(v interface{}) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		bufPool.Put(buf)
		return nil, err
	}
	raw := buf.Bytes()
	result := make([]byte, len(raw)-1)
	copy(result, raw)
	bufPool.Put(buf)
	return result, nil
}

func isConnectivityTest(messages []Message) bool {
	if len(messages) != 1 || messages[0].Role != "user" {
		return false
	}
	var parts []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(messages[0].Content, &parts) != nil || len(parts) == 0 {
		return false
	}
	return parts[0].Type == "image_url"
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
