package zbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The jar used to hand every cookie to every host, which leaked the Z.AI session
// to any client-supplied image URL.
func TestCookieJarScopesByHostAndPath(t *testing.T) {
	j := &cookieJar{}
	origin, _ := url.Parse("https://chat.z.ai/api/v1/auths")
	j.SetCookies(origin, []*http.Cookie{
		{Name: "hostonly", Value: "a"},
		{Name: "domainwide", Value: "b", Domain: ".z.ai"},
		{Name: "scoped", Value: "c", Path: "/api"},
		// An upstream must not be able to widen a cookie to a domain it does not
		// own, and a bare suffix match would accept a public suffix.
		{Name: "evil", Value: "d", Domain: ".co.uk"},
		{Name: "alsoevil", Value: "e", Domain: "attacker.example"},
	})

	names := func(raw string) []string {
		u, _ := url.Parse(raw)
		var out []string
		for _, c := range j.Cookies(u) {
			out = append(out, c.Name)
		}
		return out
	}

	got := strings.Join(names("https://chat.z.ai/api/v1/models"), ",")
	if got != "hostonly,domainwide,scoped" {
		t.Errorf("same host and path: got %q, want %q", got, "hostonly,domainwide,scoped")
	}

	// Path-scoped cookie must not travel outside its path.
	if got := strings.Join(names("https://chat.z.ai/other"), ","); got != "hostonly,domainwide" {
		t.Errorf("outside path: got %q, want %q", got, "hostonly,domainwide")
	}

	// A different host under the same domain gets only the domain-wide cookie.
	if got := strings.Join(names("https://cdn.z.ai/asset.js"), ","); got != "domainwide" {
		t.Errorf("sibling host: got %q, want %q", got, "domainwide")
	}

	// A foreign host must get nothing at all: this is the leak.
	for _, foreign := range []string{
		"https://attacker.example/x",
		"http://evil.co.uk/x",
		"http://169.254.169.254/latest/meta-data",
	} {
		if got := names(foreign); len(got) != 0 {
			t.Errorf("%s leaked cookies: %v", foreign, got)
		}
	}
}

func TestCookiePathMatches(t *testing.T) {
	cases := []struct {
		reqPath, cookiePath string
		want                bool
	}{
		{"/api/v1", "", true},
		{"/api/v1", "/", true},
		{"/api/v1", "/api", true},
		{"/api/v1", "/api/", true},
		{"/api", "/api", true},
		{"/apifoo", "/api", false},
		{"/other", "/api", false},
	}
	for _, c := range cases {
		if got := cookiePathMatches(c.reqPath, c.cookiePath); got != c.want {
			t.Errorf("cookiePathMatches(%q, %q) = %v, want %v", c.reqPath, c.cookiePath, got, c.want)
		}
	}
}

func TestIsRoutableIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", // cloud metadata
		"100.100.100.200", // carrier NAT, Alibaba metadata
		"0.0.0.0", "0.1.2.3", "255.255.255.255",
		"224.0.0.1",
		"::1", "fc00::1", "fe80::1",
		"::ffff:127.0.0.1", // IPv4-mapped loopback
		"::ffff:10.0.0.1",
		"2002:7f00:1::",   // 6to4 wrapping 127.0.0.1
		"64:ff9b::7f00:1", // NAT64 wrapping 127.0.0.1
		"64:ff9b::a00:1",  // NAT64 wrapping 10.0.0.1
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test case %q is not a valid IP", s)
		}
		if isRoutableIP(ip) {
			t.Errorf("isRoutableIP(%s) = true, want false", s)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700::1111"}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if !isRoutableIP(ip) {
			t.Errorf("isRoutableIP(%s) = false, want true", s)
		}
	}
}

func TestValidateFetchTargetRejectsNonHTTP(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "gopher://x/", "ftp://host/f"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateFetchTarget(u); err == nil {
			t.Errorf("validateFetchTarget(%q) = nil, want an error", raw)
		}
	}
	u, _ := url.Parse("https://example.com/a.png")
	if err := validateFetchTarget(u); err != nil {
		t.Errorf("validateFetchTarget on a normal URL: %v", err)
	}
}

// mimeType becomes a multipart header value, and multipart does not sanitise those.
func TestNormalizeImageMIME(t *testing.T) {
	cases := map[string]string{
		"image/png":                  "image/png",
		"image/jpeg":                 "image/jpeg",
		"image/jpg":                  "image/jpeg",
		"IMAGE/WEBP":                 "image/webp",
		"image/jpeg; charset=utf-8":  "image/jpeg",
		"image/gif;q=1":              "image/gif",
		"image/svg+xml":              "image/png",
		"image/png\r\nX-Injected: 1": "image/png",
		"text/html":                  "image/png",
		"":                           "image/png",
	}
	for in, want := range cases {
		if got := normalizeImageMIME(in); got != want {
			t.Errorf("normalizeImageMIME(%q) = %q, want %q", in, got, want)
		}
		if got := normalizeImageMIME(in); strings.ContainsAny(got, "\r\n") {
			t.Errorf("normalizeImageMIME(%q) returned CR/LF: %q", in, got)
		}
	}
}

func TestPrintableASCIINeutralisesControlBytes(t *testing.T) {
	if got := printableASCII("/v1/chat"); got != "/v1/chat" {
		t.Errorf("clean input was altered: %q", got)
	}
	got := printableASCII("/v1/\x1b[2Jchat\n")
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, '\n') {
		t.Errorf("control bytes survived: %q", got)
	}
	if got != `/v1/\x1b[2Jchat\x0a` {
		t.Errorf("got %q, want %q", got, `/v1/\x1b[2Jchat\x0a`)
	}
}

func TestRedactSecretRevealsNothingUsable(t *testing.T) {
	const token = "eyJhbGciOiJIUzI1NiJ9.payloadpayloadpayload.signaturesignature"
	got := redactSecret(token)
	if strings.Contains(got, "payloadpayload") {
		t.Errorf("redactSecret leaked the body: %q", got)
	}
	if got == token {
		t.Error("redactSecret returned the token unchanged")
	}
	if redactSecret("") != "" {
		t.Error("empty secret should stay empty")
	}
	if got := redactSecret("short"); strings.Contains(got, "short") {
		t.Errorf("a short secret should be fully masked, got %q", got)
	}
}

// A Set-Cookie that clears a value must remove the entry, not leave a live empty one
// that later requests send back as "name=".
func TestCookieJarHonoursDeletion(t *testing.T) {
	j := &cookieJar{}
	origin, _ := url.Parse("https://chat.z.ai/")
	j.SetCookies(origin, []*http.Cookie{{Name: "sess", Value: "live"}})
	if got := j.Cookies(origin); len(got) != 1 || got[0].Value != "live" {
		t.Fatalf("setup failed: %#v", got)
	}

	j.SetCookies(origin, []*http.Cookie{{Name: "sess", Value: "", MaxAge: -1}})
	if got := j.Cookies(origin); len(got) != 0 {
		t.Errorf("MaxAge<0 should delete, got %#v", got)
	}

	j.SetCookies(origin, []*http.Cookie{{Name: "sess", Value: "live"}})
	j.SetCookies(origin, []*http.Cookie{
		{Name: "sess", Expires: time.Now().Add(-time.Hour)},
	})
	if got := j.Cookies(origin); len(got) != 0 {
		t.Errorf("a past Expires should delete, got %#v", got)
	}
}

// A domain-scoped cookie set by two origins must not accumulate duplicates.
func TestCookieJarDoesNotDuplicateDomainCookies(t *testing.T) {
	j := &cookieJar{}
	a, _ := url.Parse("https://chat.z.ai/")
	b, _ := url.Parse("https://cdn.z.ai/")
	j.SetCookies(a, []*http.Cookie{{Name: "shared", Value: "one", Domain: ".z.ai"}})
	j.SetCookies(b, []*http.Cookie{{Name: "shared", Value: "two", Domain: ".z.ai"}})

	got := j.Cookies(a)
	if len(got) != 1 {
		t.Fatalf("expected one entry, got %d: %#v", len(got), got)
	}
	if got[0].Value != "two" {
		t.Errorf("expected the newer value, got %q", got[0].Value)
	}
}

// An upstream that opens a tool-call marker and never closes it must not grow the
// interceptor buffer without bound.
func TestAgentInterceptorBoundsHeldText(t *testing.T) {
	in := &AgentStreamInterceptor{}
	chunk := strings.Repeat("x", 64<<10)
	var emitted int
	for i := 0; i < 200; i++ {
		emitted += len(in.Feed(chunk).Content)
		if len(in.buffer) > maxAgentHold+len(chunk)+1 {
			t.Fatalf("buffer grew to %d, past the %d cap", len(in.buffer), maxAgentHold)
		}
	}
	if emitted == 0 {
		t.Error("plain text should have been emitted as content")
	}
}

func TestParseAuthTokens(t *testing.T) {
	cases := map[string][]string{
		"Jubin,unlimited":     {"Jubin", "unlimited"},
		" Jubin , unlimited ": {"Jubin", "unlimited"},
		"solo":                {"solo"},
		"a,,b,":               {"a", "b"},
		"   ":                 nil,
		"":                    nil,
	}
	for in, want := range cases {
		got := parseAuthTokens(in)
		if len(got) != len(want) {
			t.Errorf("parseAuthTokens(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseAuthTokens(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

func TestCheckAuthAcceptsAnyConfiguredKey(t *testing.T) {
	saved := config.Auth.Tokens
	defer func() { config.Auth.Tokens = saved }()
	config.Auth.Tokens = []string{"Jubin", "unlimited"}

	accept := func(key string) bool {
		r := httptest.NewRequest("GET", "/status", nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		return checkAuth(r)
	}

	if !accept("Jubin") {
		t.Error("Jubin should be accepted")
	}
	if !accept("unlimited") {
		t.Error("unlimited should be accepted")
	}
	if accept("wrong") {
		t.Error("an unlisted key must be rejected")
	}
	if accept("") {
		t.Error("no key must be rejected")
	}

	// x-api-key is how Anthropic clients present it.
	r := httptest.NewRequest("GET", "/status", nil)
	r.Header.Set("x-api-key", "unlimited")
	if !checkAuth(r) {
		t.Error("unlimited via x-api-key should be accepted")
	}
}

func TestExpiredTokensPurgedAndRefilledOnDemand(t *testing.T) {
	oldDBPath := dbPath
	dbPath = filepath.Join(t.TempDir(), "tokens.sqlite")
	defer func() {
		closeDB()
		dbPath = oldDBPath
	}()
	if err := initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}

	tx, err := globalDB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 1; i <= 500; i++ {
		if _, err := tx.Exec(`INSERT INTO tokens (token, batch) VALUES (?, 1)`, fmt.Sprintf("expired-tok-%d", i)); err != nil {
			t.Fatalf("insert expired: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := refreshTokenCount(); got != 500 {
		t.Fatalf("initial count = %d, want 500", got)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.FormValue("Action") {
		case "InitCaptchaV3":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"CertifyId":"cert-test-123456"}`))
		case "VerifyCaptchaV3":
			var cvp CVP
			_ = json.Unmarshal([]byte(r.FormValue("CaptchaVerifyParam")), &cvp)
			w.Header().Set("Content-Type", "application/json")
			if strings.HasPrefix(cvp.DeviceToken, "fresh-tok-") {
				_, _ = w.Write([]byte(`{"Success":true,"Result":{"VerifyResult":true,"securityToken":"sec-ok","certifyId":"cert-test-123456"}}`))
			} else {
				_, _ = w.Write([]byte(`{"Success":true,"Result":{"VerifyResult":false}}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldInit, oldVerify := aliyunInitURL, aliyunVerifyURL
	aliyunInitURL, aliyunVerifyURL = srv.URL, srv.URL
	defer func() {
		aliyunInitURL, aliyunVerifyURL = oldInit, oldVerify
	}()

	select {
	case <-tokenDemand:
	default:
	}
	collectorAvailable.Store(true)
	collectorHealthy.Store(true)
	defer func() {
		collectorAvailable.Store(false)
		collectorHealthy.Store(false)
	}()

	refillDone := make(chan struct{})
	go func() {
		defer close(refillDone)
		<-tokenDemand
		for i := 1; i <= 5; i++ {
			_, _ = globalDB.Exec(`INSERT INTO tokens (token, batch) VALUES (?, 2)`, fmt.Sprintf("fresh-tok-%d", i))
		}
		refreshTokenCount()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payload := computeFinalPayload(ctx)
	if payload == "" {
		t.Fatal("computeFinalPayload returned empty payload; expected auto-purge and refill recovery")
	}
	<-refillDone

	var expiredLeft int
	if err := globalDB.QueryRow(`SELECT COUNT(*) FROM tokens WHERE token LIKE 'expired-tok-%'`).Scan(&expiredLeft); err != nil {
		t.Fatalf("count expired: %v", err)
	}
	if expiredLeft != 0 {
		t.Errorf("expired tokens remaining = %d, want 0", expiredLeft)
	}
	if got := refreshTokenCount(); got != 4 {
		t.Errorf("remaining fresh tokens = %d, want 4", got)
	}
}

func TestProbeStoredTokensPurgesExpiredAndCachesValid(t *testing.T) {
	oldDBPath := dbPath
	dbPath = filepath.Join(t.TempDir(), "tokens.sqlite")
	defer func() {
		closeDB()
		dbPath = oldDBPath
	}()
	if err := initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}

	for i := 1; i <= 10; i++ {
		_, _ = globalDB.Exec(`INSERT INTO tokens (token, batch) VALUES (?, 1)`, fmt.Sprintf("expired-tok-%d", i))
	}
	refreshTokenCount()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.FormValue("Action") {
		case "InitCaptchaV3":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"CertifyId":"cert-probe-123"}`))
		case "VerifyCaptchaV3":
			var cvp CVP
			_ = json.Unmarshal([]byte(r.FormValue("CaptchaVerifyParam")), &cvp)
			w.Header().Set("Content-Type", "application/json")
			if strings.HasPrefix(cvp.DeviceToken, "fresh-tok-") {
				_, _ = w.Write([]byte(`{"Success":true,"Result":{"VerifyResult":true,"securityToken":"sec-ok","certifyId":"cert-probe-123"}}`))
			} else {
				_, _ = w.Write([]byte(`{"Success":true,"Result":{"VerifyResult":false}}`))
			}
		}
	}))
	defer srv.Close()

	oldInit, oldVerify := aliyunInitURL, aliyunVerifyURL
	aliyunInitURL, aliyunVerifyURL = srv.URL, srv.URL
	defer func() {
		aliyunInitURL, aliyunVerifyURL = oldInit, oldVerify
	}()

	if probeStoredTokens(context.Background()) {
		t.Fatal("probeStoredTokens should return false when stored tokens are expired")
	}
	if got := refreshTokenCount(); got != 0 {
		t.Fatalf("token count after expired probe = %d, want 0", got)
	}

	_, _ = globalDB.Exec(`INSERT INTO tokens (token, batch) VALUES ('fresh-tok-1', 2)`)
	refreshTokenCount()
	if !probeStoredTokens(context.Background()) {
		t.Fatal("probeStoredTokens should return true when stored token is valid")
	}
}
