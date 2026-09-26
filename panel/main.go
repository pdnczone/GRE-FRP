package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
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

type peerConfig struct {
	Addr string `json:"addr"`
	User string `json:"user"`
	Pass string `json:"pass"`
}

var (
	cfg   panelConfig
	peer  peerConfig
	mu    sync.Mutex
	nonce [32]byte
)

func cfgPath() string { return filepath.Join(configDir, "panel.json") }
func peerPath() string { return filepath.Join(configDir, "peer.json") }

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

func loadPeer() {
	data, err := os.ReadFile(peerPath())
	if err == nil {
		_ = json.Unmarshal(data, &peer)
	}
}

func main() {
	if v := os.Getenv("GRE_PANEL_DIR"); v != "" {
		configDir = v
	}
	loadOrInit()
	loadPeer()
	if _, err := rand.Read(nonce[:]); err != nil {
		log.Fatal(err)
	}

	base := "/" + cfg.BasePath
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"/", serveIndex)
	mux.HandleFunc("GET "+base+"/api/status", requireAuth(handleStatus))
	mux.HandleFunc("POST "+base+"/api/login", handleLogin)
	mux.HandleFunc("POST "+base+"/api/logout", handleLogout)
	mux.HandleFunc("GET "+base+"/api/logs", requireAuth(handleLogs))
	mux.HandleFunc("POST "+base+"/api/action", requireAuth(handleAction))
	mux.HandleFunc("GET "+base+"/api/peer", requireAuth(handlePeerGet))
	mux.HandleFunc("POST "+base+"/api/peer", requireAuth(handlePeerSet))
	mux.HandleFunc("POST "+base+"/api/password", requireAuth(handlePassword))

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("gre-panel listening on %s under /%s", addr, cfg.BasePath)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ---- auth (cookie + csrf, inspired by hashem webui) ----

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
	if _, err := rand.Read(nonce[:]); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	mac := sha256.Sum256(append(nonce[:], []byte(cfg.PassHash)...))
	http.SetCookie(w, sessionCookie(hex.EncodeToString(mac[:]), 86400*7))
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- pages & api ----

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

// status of GRE + FRP on this machine, plus remote peer if configured.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	local := localStatus()
	out := map[string]any{"local": local}
	if peer.Addr != "" {
		out["peer"] = remoteStatus()
	}
	writeJSON(w, out)
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("svc")
	if svc != "frps" && svc != "frpc" {
		svc = "frps"
	}
	if _, err := exec.LookPath("journalctl"); err == nil {
		out, err := exec.Command("journalctl", "-u", svc, "-n", "50", "--no-pager").CombinedOutput()
		if err == nil {
			writeJSON(w, map[string]string{"logs": string(out)})
			return
		}
	}
	// fallback: log files
	for _, p := range []string{"/var/log/" + svc + ".log", "/root/" + svc + ".log"} {
		if data, err := os.ReadFile(p); err == nil {
			writeJSON(w, map[string]string{"logs": string(data)})
			return
		}
	}
	writeJSON(w, map[string]string{"logs": "(no logs available)"})
}

// actions: restart frps/frpc/gre, ping peer.
func handleAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch body.Action {
	case "restart-frps", "restart-frpc", "restart-gre", "ping":
		out, err := runAction(body.Action)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"status": "ok", "output": out})
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

func runAction(action string) (string, error) {
	switch action {
	case "restart-frps":
		out, err := exec.Command("systemctl", "restart", "frps").CombinedOutput()
		return string(out), err
	case "restart-frpc":
		out, err := exec.Command("systemctl", "restart", "frpc").CombinedOutput()
		return string(out), err
	case "restart-gre":
		out, err := exec.Command("systemctl", "restart", "gre-tunnel.service").CombinedOutput()
		return string(out), err
	case "ping":
		st := localStatus()
		if st.GrePeer == "" {
			return "", fmt.Errorf("no GRE peer known")
		}
		out, err := exec.Command("ping", "-c", "3", "-W", "2", st.GrePeer).CombinedOutput()
		return string(out), err
	}
	return "", fmt.Errorf("unknown action")
}

func handlePeerGet(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	writeJSON(w, map[string]string{"addr": peer.Addr, "user": peer.User})
}

func handlePeerSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Addr string `json:"addr"`
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Addr) == "" {
		http.Error(w, "peer address is required", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	peer = peerConfig{Addr: strings.TrimSpace(body.Addr), User: body.User, Pass: body.Pass}
	_ = os.WriteFile(peerPath(), mustJSON(peer), 0600)
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- local inspection (reads systemd + ip, never writes except via actions) ----

type greState struct {
	Exists bool   `json:"exists"`
	Name   string `json:"name"`
	Local  string `json:"local"`
	PeerIP string `json:"peer_ip"`
	Inner  string `json:"inner"`
}

type tunnelStatus struct {
	Role     string   `json:"role"`
	Gre      greState `json:"gre"`
	GrePeer  string   `json:"gre_peer"`
	PingOK   bool     `json:"ping_ok"`
	PingMs   string   `json:"ping_ms"`
	FrpUp    bool     `json:"frp_up"`
	FrpSvc   string   `json:"frp_svc"`
	FrpPort  int      `json:"frp_port"`
	Proxies  []string `json:"proxies"`
	BindPort int      `json:"bind_port"`
}

func localStatus() tunnelStatus {
	var st tunnelStatus
	// GRE interface
	if out, err := exec.Command("ip", "tunnel", "show").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "gre-tunnel") {
				st.Gre.Exists = true
				st.Gre.Name = "gre-tunnel"
				parts := strings.Fields(line)
				for i, p := range parts {
					if p == "local" && i+1 < len(parts) {
						st.Gre.Local = parts[i+1]
					}
					if p == "remote" && i+1 < len(parts) {
						st.Gre.PeerIP = parts[i+1]
						st.GrePeer = parts[i+1]
					}
				}
			}
		}
	}
	if out, err := exec.Command("ip", "-4", "addr", "show", "dev", "gre-tunnel").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "inet ") {
				st.Gre.Inner = strings.Fields(line)[1]
			}
		}
	}
	// FRP role: which unit file exists / is active
	for _, svc := range []string{"frps", "frpc"} {
		if out, err := exec.Command("systemctl", "is-active", svc).CombinedOutput(); err == nil &&
			strings.TrimSpace(string(out)) == "active" {
			st.FrpUp = true
			st.FrpSvc = svc
			if svc == "frps" {
				st.Role = "iran (server)"
			} else {
				st.Role = "foreign (client)"
			}
			break
		}
	}
	if st.Role == "" {
		// fall back to config presence
		if _, err := os.Stat("/etc/frp/frps.toml"); err == nil {
			st.Role = "iran (server)"
			st.FrpSvc = "frps"
		} else if _, err := os.Stat("/etc/frp/frpc.toml"); err == nil {
			st.Role = "foreign (client)"
			st.FrpSvc = "frpc"
		}
	}
	// ports & proxies from toml
	tomlPath := "/etc/frp/frps.toml"
	if st.FrpSvc == "frpc" {
		tomlPath = "/etc/frp/frpc.toml"
	}
	if data, err := os.ReadFile(tomlPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "bindPort") || strings.HasPrefix(line, "serverPort") {
				var v int
				fmt.Sscanf(line, "%*s = %d", &v)
				st.BindPort = v
				st.FrpPort = v
			}
			if strings.HasPrefix(line, "name = ") {
				name := strings.Trim(strings.TrimPrefix(line, "name = "), `"`)
				st.Proxies = append(st.Proxies, name)
			}
		}
	}
	// quick ping to GRE peer inner ip
	if st.Gre.Inner != "" {
		target := grePeerInner(st.Gre.Inner)
		if target != "" {
			start := time.Now()
			if err := exec.Command("ping", "-c", "1", "-W", "2", target).Run(); err == nil {
				st.PingOK = true
				st.PingMs = fmt.Sprintf("%.0fms", float64(time.Since(start).Microseconds())/1000)
			}
		}
	}
	return st
}

// grePeerInner flips the last bit of a /30 inner address.
func grePeerInner(cidr string) string {
	ip := strings.Split(cidr, "/")[0]
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	last := 0
	fmt.Sscanf(parts[3], "%d", &last)
	if last%2 == 0 {
		last--
	} else {
		last++
	}
	return fmt.Sprintf("%s.%s.%s.%d", parts[0], parts[1], parts[2], last)
}

// remoteStatus asks the peer panel for its status over its secret path.
func remoteStatus() any {
	mu.Lock()
	p := peer
	mu.Unlock()
	if p.Addr == "" {
		return map[string]string{"error": "peer not configured"}
	}
	base := strings.TrimRight(p.Addr, "/")
	// login to peer panel, then fetch status — best effort, short timeouts
	client := &http.Client{Timeout: 8 * time.Second}
	// NOTE: peer.Addr must include the secret base path, e.g.
	// http://10.10.10.1:7777/<secret>
	loginBody := fmt.Sprintf(`{"username":%q,"password":%q}`, p.User, p.Pass)
	resp, err := client.Post(base+"/api/login", "application/json", strings.NewReader(loginBody))
	if err != nil {
		return map[string]string{"error": "peer unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return map[string]string{"error": "peer login failed"}
	}
	var session string
	for _, c := range resp.Cookies() {
		if c.Name == "gre_session" {
			session = c.Value
		}
	}
	req, _ := http.NewRequest("GET", base+"/api/status", nil)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "gre_session", Value: session})
	}
	r2, err := client.Do(req)
	if err != nil {
		return map[string]string{"error": "peer status failed: " + err.Error()}
	}
	defer r2.Body.Close()
	var out any
	if err := json.NewDecoder(r2.Body).Decode(&out); err != nil {
		return map[string]string{"error": "peer bad response"}
	}
	return out
}
