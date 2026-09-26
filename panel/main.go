package main

// Entry point: config load, route table, static assets.
// Auth lives in auth.go, tunnel status/actions in tunnel.go,
// setup in setup.go, dashboard metrics in dashboard.go.

import (
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

//go:embed index.html tokens.css base.css favicon.png
var panelFS embed.FS

var configDir = "/etc/gre-panel"

type panelConfig struct {
	Username string `json:"username"`
	PassHash string `json:"pass_hash"`
	Port     int    `json:"port"`
	BasePath string `json:"base_path"`
}

var (
	cfg panelConfig
	// panelVersion is set at release build time:
	// go build -ldflags "-X main.panelVersion=panel-rN"
	panelVersion = "dev"
)

func cfgPath() string { return filepath.Join(configDir, "panel.json") }

func loadOrInit() {
	_ = os.MkdirAll(configDir, 0700)
	data, err := os.ReadFile(cfgPath())
	if err == nil && json.Unmarshal(data, &cfg) == nil && cfg.PassHash != "" {
		// Test/dev override: fixed password via env (takes effect on restart).
		if pw := os.Getenv("GRE_PANEL_PASSWORD"); pw != "" {
			h := sha256.Sum256([]byte(pw))
			cfg.PassHash = hex.EncodeToString(h[:])
			_ = os.WriteFile(cfgPath(), mustJSON(cfg), 0600)
		}
		return
	}
	pass := os.Getenv("GRE_PANEL_PASSWORD")
	if pass == "" {
		pass = randomDigits(8)
	}
	h := sha256.Sum256([]byte(pass))
	cfg = panelConfig{
		Username: "admin",
		PassHash: hex.EncodeToString(h[:]),
		Port:     7777,
		BasePath: randomBase(12),
	}
	_ = os.WriteFile(cfgPath(), mustJSON(cfg), 0600)
	// plaintext copy so the server admin can view it later via script menu (user choice)
	_ = os.WriteFile(filepath.Join(configDir, "panel.pass"), []byte(pass), 0600)
	log.Printf("panel password: %s (user %s) — change it from Settings", pass, cfg.Username)
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}

func randomDigits(n int) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	digits := "0123456789"
	out := make([]byte, n)
	for i := range out {
		out[i] = digits[int(b[i])%10]
	}
	return string(out)
}

func randomBase(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v" || os.Args[1] == "version") {
		fmt.Println(panelVersion)
		return
	}
	if v := os.Getenv("GRE_PANEL_DIR"); v != "" {
		configDir = v
	}
	loadOrInit()
	if _, err := rand.Read(nonce[:]); err != nil {
		log.Fatal(err)
	}

	base := "/" + cfg.BasePath
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"/", serveIndex)
	mux.HandleFunc("GET "+base+"/tokens.css", serveAsset("tokens.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET "+base+"/base.css", serveAsset("base.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET "+base+"/favicon.png", serveAsset("favicon.png", "image/png"))
	mux.HandleFunc("GET "+base+"/api/health", handleHealth)
	mux.HandleFunc("GET "+base+"/api/status", requireAuth(handleStatus))
	mux.HandleFunc("GET "+base+"/api/dashboard", requireAuth(handleDashboard))
	mux.HandleFunc("POST "+base+"/api/login", handleLogin)
	mux.HandleFunc("POST "+base+"/api/logout", handleLogout)
	mux.HandleFunc("GET "+base+"/api/logs", requireAuth(handleLogs))
	mux.HandleFunc("POST "+base+"/api/action", requireAuth(handleAction))
	mux.HandleFunc("POST "+base+"/api/password", requireAuth(handlePassword))
	mux.HandleFunc("GET "+base+"/api/setup", requireAuth(handleSetupGet))
	mux.HandleFunc("GET "+base+"/api/version", requireAuth(handleVersion))
	mux.HandleFunc("POST "+base+"/api/update", requireAuth(handleUpdate))
	mux.HandleFunc("POST "+base+"/api/setup", requireAuth(handleSetupPost))
	mux.HandleFunc("GET "+base+"/api/peers", requireAuth(handlePeersGet))
	mux.HandleFunc("POST "+base+"/api/peers", requireAuth(handlePeersPost))

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("gre-panel listening on %s under /%s", addr, cfg.BasePath)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func serveAsset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := panelFS.ReadFile(name)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := panelFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	page := strings.ReplaceAll(string(data), "__BASE_PATH__", "/"+cfg.BasePath)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleHealth is unauthenticated: lets browsers/proxies verify the panel
// is reachable without exposing any data.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "version": panelVersion})
}
