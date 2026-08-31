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
	"syscall"
	"time"

	utls "github.com/refraction-networking/utls"
)

func init() {
	// Safe-character table for the URL encoder below.
	for i := 0; i < 256; i++ {
		c := byte(i)
		if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			baseSafeTable[i] = true
		}
	}
}

// Message-only shorthands over the levelled logger.
func logError(msg string) { logErrorf("%s", msg) }
func logInfo(msg string)  { logInfof("%s", msg) }

var bufPool = sync.Pool{
	New: func() interface{} { return bytes.NewBuffer(make([]byte, 0, 4096)) },
}

var zlibWriterPool = sync.Pool{
	New: func() interface{} {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.DefaultCompression)
		return w
	},
}

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

// TLS fingerprint spoofing: uTLS with a Chrome ClientHello.
//
// Aliyun's ESA WAF fingerprints JA3 and blocks Go's default TLS stack.

// Shared, because a net.Dialer holds no per-connection state.
var utlsDialer = &net.Dialer{
	Timeout:   15 * time.Second,
	KeepAlive: 30 * time.Second,
}

// Resolved once, not six env lookups per dial.
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

// dialUTLS opens a TLS connection with a Chrome ClientHello, tunnelling through
// HTTP(S)_PROXY when set.
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

	// HTTP/1.1 only: negotiating h2 would change the fingerprint.
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

// dialViaProxy opens an HTTP CONNECT tunnel to addr through proxyURL.
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

	// The reader may have swallowed bytes belonging to the TLS handshake.
	if br.Buffered() > 0 {
		buffered := make([]byte, br.Buffered())
		br.Read(buffered)
		return &concatConn{Conn: proxyConn, buffer: buffered}, nil
	}
	return proxyConn, nil
}

// concatConn replays bytes a bufio.Reader read ahead of the caller.
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

// Z.AI client: cookie jar plus the uTLS Chrome fingerprint.
var zaiJar = &cookieJar{}

// Z.AI upstream. The dialer negotiates HTTP/1.1 only, so one connection carries
// one stream and MaxConnsPerHost is the real ceiling on concurrent completions —
// past it requests block inside the transport with no error to report.
//
// No client Timeout: responses are streamed and each request carries its own
// deadline. ResponseHeaderTimeout bounds the part that should be fast, so a
// stalled upstream fails quickly instead of holding a session.
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

type cookieEntry struct {
	name   string
	value  string
	domain string
	path   string
	// Origin host, so a Set-Cookie with no Domain stays host-only.
	host string
}

type cookieJar struct {
	mu      sync.Mutex
	cookies []cookieEntry
}

func (j *cookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var origin string
	if u != nil {
		origin = strings.ToLower(u.Hostname())
	}
	for _, c := range cookies {
		// A response may only widen a cookie to a domain it belongs to; otherwise a
		// suffix match would honour something as broad as ".co.uk".
		if c.Domain != "" && !cookieHostMatches(origin, cookieEntry{domain: c.Domain}) {
			continue
		}
		// host identifies host-only cookies only: including it for a domain-scoped
		// one let two origins each keep a copy, sending it twice with a stale value.
		filtered := j.cookies[:0]
		for _, e := range j.cookies {
			same := e.name == c.Name && e.domain == c.Domain && e.path == c.Path
			if same && (c.Domain != "" || e.host == origin) {
				continue
			}
			filtered = append(filtered, e)
		}
		j.cookies = filtered

		// A server clearing a cookie must have it removed, not stored as a live
		// empty value that we then send back as "name=".
		if c.MaxAge < 0 || (!c.Expires.IsZero() && !c.Expires.After(time.Now())) {
			continue
		}

		j.cookies = append(j.cookies, cookieEntry{
			name:   c.Name,
			value:  c.Value,
			domain: c.Domain,
			path:   c.Path,
			host:   origin,
		})
	}
}

// Cookies returns only the entries scoped to u. Returning the whole jar meant any
// outbound request carried the Z.AI session cookies to an arbitrary host.
func (j *cookieJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.cookies) == 0 || u == nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	reqPath := u.Path
	if reqPath == "" {
		reqPath = "/"
	}

	matched := make([]cookieEntry, 0, len(j.cookies))
	for _, e := range j.cookies {
		if cookieHostMatches(host, e) && cookiePathMatches(reqPath, e.path) {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		return nil
	}

	// One backing array, not a pointer slice grown per request.
	out := make([]*http.Cookie, len(matched))
	storage := make([]http.Cookie, len(matched))
	for i, e := range matched {
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

func cookieHostMatches(host string, e cookieEntry) bool {
	domain := strings.ToLower(strings.TrimPrefix(e.domain, "."))
	if domain == "" {
		// Host-only: exact match against the host that set it.
		return host != "" && host == e.host
	}
	if host == domain {
		return true
	}
	return strings.HasSuffix(host, "."+domain)
}

func cookiePathMatches(reqPath, cookiePath string) bool {
	if cookiePath == "" || cookiePath == "/" {
		return true
	}
	if reqPath == cookiePath {
		return true
	}
	if !strings.HasPrefix(reqPath, cookiePath) {
		return false
	}
	// "/foo" must not match "/foobar", but must match "/foo/bar".
	if strings.HasSuffix(cookiePath, "/") {
		return true
	}
	return reqPath[len(cookiePath)] == '/'
}

func randomUUID() string { return generateUUID() }

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// generateUUID builds a v4 UUID by hand, avoiding fmt.Sprintf.
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

// estimateTokens approximates at four bytes per token.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

// getMessageContent flattens a content field (plain string or typed parts).
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

// lastUserPromptText returns the most recent user message text, used as
// signature_prompt so the upstream signs the latest turn, not the whole thread.
func lastUserPromptText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return getMessageContent(messages[i].Content)
		}
	}
	return ""
}

// URL encoding via lookup table, no per-character allocation: this runs over
// every captcha parameter on the request path.

const hexUpper = "0123456789ABCDEF"
const hexLower = "0123456789abcdef"

var baseSafeTable [256]bool

func urlEncode(s string) string {
	escaped := 0
	for i := 0; i < len(s); i++ {
		if !baseSafeTable[s[i]] {
			escaped++
		}
	}
	if escaped == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + escaped*2)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if baseSafeTable[c] {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0x0F])
		}
	}
	return b.String()
}

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

// jsonMarshal encodes v without HTML escaping through a pooled buffer, trimming
// the newline json.Encoder appends.
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

// redactSecret fingerprints a credential: enough to tell two apart, not to use one.
func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "..." + s[len(s)-4:] + fmt.Sprintf("(%d)", len(s))
}

// Client-supplied image URLs use this, never zaiHTTPClient: no cookie jar, so nothing
// carries Z.AI credentials to a foreign host. The address check sits in the dialer's
// Control hook, which sees the peer the connection actually uses — resolving the name
// separately is a TOCTOU a rebinding record walks straight through.
var imageFetchClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if ip == nil {
					return fmt.Errorf("refusing unresolved address %q", address)
				}
				if !isRoutableIP(ip) {
					return fmt.Errorf("refusing to connect to non-routable address %s", ip)
				}
				return nil
			},
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        8,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		return validateFetchTarget(req.URL)
	},
}

// validateFetchTarget rejects URL shapes outright; the dialer handles addresses.
func validateFetchTarget(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported image URL scheme %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("image URL has no host")
	}
	return nil
}

// isRoutableIP reports whether ip is public. Without it the proxy forwards requests
// into its own network: cloud metadata, loopback admin ports, RFC1918 hosts.
func isRoutableIP(ip net.IP) bool {
	// Unwrap 6to4 (2002::/16) and NAT64 (64:ff9b::/96), which can carry a private
	// v4 address that the v6 predicates below would not recognise.
	if v4 := embeddedIPv4(ip); v4 != nil {
		ip = v4
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		// 0.0.0.0/8: 0.1.2.3 reaches loopback on some stacks.
		case v4[0] == 0:
			return false
		// 100.64.0.0/10, carrier NAT, which IsPrivate does not cover.
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127:
			return false
		case v4[0] == 255 && v4[1] == 255 && v4[2] == 255 && v4[3] == 255:
			return false
		}
	}
	return true
}

func embeddedIPv4(ip net.IP) net.IP {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return nil
	}
	// 2002:V4::/16
	if v6[0] == 0x20 && v6[1] == 0x02 {
		return net.IPv4(v6[2], v6[3], v6[4], v6[5])
	}
	// 64:ff9b::V4
	if v6[0] == 0x00 && v6[1] == 0x64 && v6[2] == 0xff && v6[3] == 0x9b {
		return net.IPv4(v6[12], v6[13], v6[14], v6[15])
	}
	return nil
}

// printableASCII neutralises control bytes in client- or upstream-controlled values
// before they reach a log line: r.URL.Path arrives percent-decoded, so %1b[2J would
// drive the operator's terminal. Rendered as \xNN so the evidence survives.
func printableASCII(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			fmt.Fprintf(&b, `\x%02x`, c)
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
