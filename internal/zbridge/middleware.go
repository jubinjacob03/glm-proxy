package zbridge

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Include-All-Features, x-api-key, anthropic-version")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func checkAuth(r *http.Request) bool {
	if !config.Auth.Enabled {
		return true
	}
	authHeader := r.Header.Get("Authorization")
	provided := authHeader
	if len(authHeader) >= 7 && strings.EqualFold(authHeader[:7], "Bearer ") {
		provided = authHeader[7:]
	}
	// Anthropic clients send the key as x-api-key.
	if provided == "" {
		provided = r.Header.Get("x-api-key")
	}
	// Constant-time and no early return, so neither the matching prefix length nor
	// which key matched leaks through response timing.
	match := 0
	for _, tok := range config.Auth.Tokens {
		match |= subtle.ConstantTimeCompare([]byte(provided), []byte(tok))
	}
	return match == 1
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !config.Auth.Enabled {
			next(w, r)
			return
		}
		if !checkAuth(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "error",
				"error": map[string]interface{}{
					"type":    "authentication_error",
					"message": "Invalid or missing authentication token",
				},
			})
			return
		}
		next(w, r)
	}
}

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/health", http.StatusFound)
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	authenticated := checkAuth(r)

	session.mu.Lock()
	initialized := session.Initialized
	var userID, userName, feVersion string
	var features Features
	if authenticated {
		userID = session.UserID
		userName = session.UserName
		feVersion = session.FeVersion
		features = session.Features
	}
	session.mu.Unlock()

	body := map[string]interface{}{
		"connected":   initialized,
		"mode":        "direct",
		"sessionPool": sessionPoolStatus(),
	}

	// Identity only for an authenticated caller: this route stays open for probes
	// and the dashboard, so on a 0.0.0.0 bind it must not name the Z.AI account to
	// anyone who can reach the port.
	if authenticated {
		var userIDPreview interface{}
		if userID != "" {
			if len(userID) > 8 {
				userID = userID[:8]
			}
			userIDPreview = userID + "..."
		}
		body["userName"] = userName
		body["userId"] = userIDPreview
		body["feVersion"] = feVersion
		body["features"] = features
	}

	writeJSON(w, 200, body)
}
