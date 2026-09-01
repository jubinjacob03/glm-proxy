package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// liveTokenShape mirrors a token captured from chat.z.ai with --diagnose:
// region=SG_WEB sessionId=81ch blob=856ch gatherCost=1107 md5=32ch.
func liveTokenShape() string {
	fields := []string{
		"SG_WEB",
		"3795d28242a11619bc25f786f84e53d4-h-1782531783720-ac9e47a76eee443087943a278f191642",
		strings.Repeat("W", 856),
		"1107",
		"d769460d135e774310d665c292c41e95",
	}
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(fields, "#")))
}

func TestValidDeviceTokenAcceptsLiveShape(t *testing.T) {
	if !validDeviceToken(liveTokenShape()) {
		t.Fatal("live token shape rejected")
	}
}

func TestValidDeviceTokenRejectsJunk(t *testing.T) {
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cases := map[string]string{
		"empty":            "",
		"not base64":       "!!!!not-base64!!!!",
		"undefined":        encode("undefined"),
		"null":             encode("null"),
		"too few fields":   encode("SG_WEB#sid#blob#1107"),
		"too many fields":  encode("SG_WEB#sid#blob#1107#" + strings.Repeat("a", 32) + "#extra"),
		"empty region":     encode("#sid#blob#1107#" + strings.Repeat("a", 32)),
		"empty session":    encode("SG_WEB##blob#1107#" + strings.Repeat("a", 32)),
		"empty blob":       encode("SG_WEB#sid##1107#" + strings.Repeat("a", 32)),
		"short md5":        encode("SG_WEB#sid#blob#1107#abc"),
		"non-hex md5":      encode("SG_WEB#sid#blob#1107#" + strings.Repeat("z", 32)),
		"uppercase md5":    encode("SG_WEB#sid#blob#1107#" + strings.Repeat("A", 32)),
		"go printed value": "%!s(<nil>)",
	}
	for name, token := range cases {
		if validDeviceToken(token) {
			t.Errorf("%s: accepted an invalid token", name)
		}
	}
}

// The URLs here were captured live with --diagnose; every one of them has to
// survive --block-trackers or window.z_um never appears.
func TestURLAllowedCoversLiveCaptchaChain(t *testing.T) {
	required := []string{
		"https://chat.z.ai/",
		"https://z-cdn.chatglm.cn/z-ai/frontend/prod-fe-1.1.92/assets/index-CrxzZnYM.js",
		"https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js",
		"https://cloudauth-device-dualstack.ap-southeast-1.aliyuncs.com/",
		"https://g.alicdn.com/captcha-frontend/FeiLin/1.5.1/feilin004.6fbec6bbc109e4f1481e456bec5660129c5b36333bdc0e4060982d4215f94059.js",
		"https://g.alicdn.com/captcha-frontend/dynamicJS/3.29.0/pe.051.795a5cae29cbf75e.js",
		"https://g.alicdn.com/captcha-frontend/dynamicJS/3.29.0/pe.090.07757a4cbeb621e7.js",
		"https://g.alicdn.com/captcha-frontend/dynamicJS/3.29.0/main.css",
		"https://no8xfe.captcha-open-southeast.aliyuncs.com/",
		"https://no8xfe-verify.captcha-open-southeast.aliyuncs.com/",
		"https://upload.captcha-open-southeast.aliyuncs.com/",
	}
	for _, u := range required {
		if !urlAllowed(u) {
			t.Errorf("blocked a required captcha dependency: %s", u)
		}
	}
}

func TestURLAllowedStillBlocksUnrelatedHosts(t *testing.T) {
	blocked := []string{
		"https://example.com/",
		"http://chat.z.ai/",
		"https://evil.com/captcha-frontend/FeiLin/1.5.1/feilin004.a.js",
		"https://g.alicdn.com/captcha-frontend/dynamicJS/3.29.0/../../secret.txt",
		"https://sdk.rum.aliyuncs.com/v2/browser-sdk.js",
		"https://chat.z.ai.evil.com/",
	}
	for _, u := range blocked {
		if urlAllowed(u) {
			t.Errorf("allowed an unrelated URL: %s", u)
		}
	}
}
