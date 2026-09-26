package main

// Update API: check for a newer prebuilt panel release and install it.
// Same safety rules as gre.sh update_all(): verify download, keep local
// config (panel.json / panel.pass) untouched, restart the service, and
// never leave the system in a broken state on failure.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

var updateClient = &http.Client{Timeout: 25 * time.Second}

// handleVersion reports the running build and the latest GitHub release.
func handleVersion(w http.ResponseWriter, r *http.Request) {
	latest, _ := latestReleaseTag()
	out := map[string]any{"current": panelVersion, "latest": latest}
	if latest != "" && latest != panelVersion {
		out["update_available"] = true
	} else {
		out["update_available"] = false
	}
	writeJSON(w, out)
}

// latestReleaseTag asks the GitHub API for the newest panel-rN tag.
// Empty string = could not determine (offline / rate-limited); the
// frontend then shows "unknown" instead of failing.
func latestReleaseTag() (string, error) {
	resp, err := updateClient.Get("https://api.github.com/repos/pdnczone/hashem-panel/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api: %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// handleUpdate downloads the latest prebuilt binary for this arch,
// verifies it (non-empty ELF), swaps it in, and restarts the service.
// panel.json / panel.pass are never touched, so local credentials survive.
func handleUpdate(w http.ResponseWriter, r *http.Request) {
	arch, asset, err := panelAsset()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = arch
	latest, err := latestReleaseTag()
	if err != nil || latest == "" {
		http.Error(w, "cannot check latest release (network or GitHub API unavailable)", http.StatusBadGateway)
		return
	}
	if latest == panelVersion {
		writeJSON(w, map[string]string{"status": "ok", "detail": "already latest (" + panelVersion + ")"})
		return
	}
	dlURL := "https://github.com/pdnczone/hashem-panel/releases/download/" + latest + "/" + asset
	tmp, err := os.CreateTemp("", "gre-panel-update-*")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := downloadFile(dlURL, tmp); err != nil {
		http.Error(w, "download failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := verifyELF(tmpPath); err != nil {
		http.Error(w, "downloaded file failed verification: "+err.Error(), http.StatusBadGateway)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Swap in the new binary. Keep a .bak so a bad binary can be rolled back.
	bak := exe + ".bak"
	_ = os.Remove(bak)
	if err := os.Rename(exe, bak); err != nil {
		http.Error(w, "cannot replace binary: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := copyFile(tmpPath, exe); err != nil {
		_ = os.Rename(bak, exe) // roll back
		http.Error(w, "cannot install new binary, rolled back: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Chmod(exe, 0755)
	writeJSON(w, map[string]string{"status": "ok", "detail": "updated to " + latest + " — restarting panel"})
	go func() {
		time.Sleep(500 * time.Millisecond)
		restartSelf()
	}()
}

// panelAsset maps runtime arch to the release asset name.
func panelAsset() (arch, asset string, err error) {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64", "gre-panel-linux-amd64", nil
	case "arm64":
		return "arm64", "gre-panel-linux-arm64", nil
	}
	return "", "", fmt.Errorf("unsupported arch for update: %s", runtime.GOARCH)
}

func downloadFile(url string, tmp *os.File) error {
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %s", resp.Status)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return err
	}
	return tmp.Close()
}

// verifyELF rejects empty files and non-ELF downloads (e.g. an HTML
// error page from a stale release redirect).
func verifyELF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 4)
	n, err := io.ReadFull(f, magic)
	if err != nil || n != 4 {
		return fmt.Errorf("file too small")
	}
	if magic[0] != 0x7f || magic[1] != 'E' || magic[2] != 'L' || magic[3] != 'F' {
		return fmt.Errorf("not an ELF binary")
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// restartSelf restarts the systemd unit when present, otherwise re-execs
// the new binary in place (dev / non-systemd environments).
func restartSelf() {
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = exec.Command("systemctl", "restart", "gre-panel").Run()
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	// Best effort: start the new binary; the old process exits.
	_ = exec.Command(exe).Start()
	os.Exit(0)
}

// sessionFile returns the path of the server-side session store.
func sessionFile() string { return filepath.Join(configDir, "sessions.json") }

// sessionStore is the persisted set of valid session tokens.
type sessionStore struct {
	Tokens map[string]int64 `json:"tokens"` // token -> expires unix
}

func loadSessions() sessionStore {
	s := sessionStore{Tokens: map[string]int64{}}
	data, err := os.ReadFile(sessionFile())
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s)
	if s.Tokens == nil {
		s.Tokens = map[string]int64{}
	}
	return s
}

func (s sessionStore) save() {
	_ = os.WriteFile(sessionFile(), mustJSON(s), 0600)
}

// pruneExpired drops expired tokens; true if anything changed.
func (s sessionStore) pruneExpired() bool {
	now := time.Now().Unix()
	changed := false
	for tok, exp := range s.Tokens {
		if exp < now {
			delete(s.Tokens, tok)
			changed = true
		}
	}
	return changed
}

// sessionLifetime is 30 days; each authenticated request extends it.
const sessionLifetime = int64(30 * 24 * 3600)

func validSession(token string) bool {
	if token == "" {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	s := loadSessions()
	exp, ok := s.Tokens[token]
	if !ok || exp < time.Now().Unix() {
		return false
	}
	// Sliding expiration: extend on every use.
	s.Tokens[token] = time.Now().Unix() + sessionLifetime
	s.save()
	return true
}

func addSession(token string) {
	mu.Lock()
	defer mu.Unlock()
	s := loadSessions()
	s.pruneExpired()
	s.Tokens[token] = time.Now().Unix() + sessionLifetime
	s.save()
}

func dropSession(token string) {
	mu.Lock()
	defer mu.Unlock()
	s := loadSessions()
	delete(s.Tokens, token)
	s.save()
}

func dropAllSessions() {
	mu.Lock()
	defer mu.Unlock()
	sessionStore{Tokens: map[string]int64{}}.save()
}

func sessionCount() int {
	mu.Lock()
	defer mu.Unlock()
	s := loadSessions()
	if s.pruneExpired() {
		s.save()
	}
	n := 0
	for range s.Tokens {
		n++
	}
	return n
}

