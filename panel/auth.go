package main

// Auth: cookie session + login/logout/password.
// Session token = sha256(nonce + pass_hash); nonce is generated at startup
// (see main.go). Changing the password invalidates all sessions.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

var (
	mu    sync.Mutex
	nonce [32]byte
)

func sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     "gre_session",
		Value:    value,
		Path:     "/" + cfg.BasePath + "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

func authed(r *http.Request) bool {
	c, err := r.Cookie("gre_session")
	if err != nil || c.Value == "" {
		return false
	}
	mac := sha256.Sum256(append(nonce[:], []byte(cfg.PassHash)...))
	want := hex.EncodeToString(mac[:])
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h := sha256.Sum256([]byte(body.Password))
	got := hex.EncodeToString(h[:])
	if subtle.ConstantTimeCompare([]byte(body.Username), []byte(cfg.Username)) != 1 ||
		subtle.ConstantTimeCompare([]byte(got), []byte(cfg.PassHash)) != 1 {
		http.Error(w, "wrong username or password", http.StatusUnauthorized)
		return
	}
	mac := sha256.Sum256(append(nonce[:], []byte(cfg.PassHash)...))
	http.SetCookie(w, sessionCookie(hex.EncodeToString(mac[:]), 86400*7))
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, sessionCookie("", -1))
	writeJSON(w, map[string]string{"status": "ok"})
}

func handlePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Password) < 4 {
		http.Error(w, "password must be at least 4 characters", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	h := sha256.Sum256([]byte(body.Password))
	cfg.PassHash = hex.EncodeToString(h[:])
	_ = os.WriteFile(cfgPath(), mustJSON(cfg), 0600)
	// keep plaintext copy in sync (user choice: viewable via script menu)
	_ = os.WriteFile(filepath.Join(configDir, "panel.pass"), []byte(body.Password), 0600)
	if _, err := rand.Read(nonce[:]); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	mac := sha256.Sum256(append(nonce[:], []byte(cfg.PassHash)...))
	http.SetCookie(w, sessionCookie(hex.EncodeToString(mac[:]), 86400*7))
	writeJSON(w, map[string]string{"status": "ok"})
}
