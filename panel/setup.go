package main

// Setup API: validate the web form, then run the SAME gre.sh install
// functions the CLI/menu use (single source of truth). The request fields
// map 1:1 to the setup-iran / setup-foreign CLI flags at the bottom of
// gre.sh, and the installer script path is resolved next to the binary so it
// works both in dev (./panel/gre.sh) and on servers (/usr/local/bin/).
//
// Iran side: GRE + frps (token auto-generated, shown for copy to Foreign).
// Foreign side: GRE + frpc (token entered manually, ports like "443, 2083").

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultIranGRE    = "10.10.10.2"
	defaultForeignGRE = "10.10.10.1"
	defaultFrpPort    = 7000
)

// ---- gre.sh location ----

// greScriptPath finds the installer: GRE_SCRIPT env wins, else <bindir>/gre.sh
// (servers: alongside /usr/local/bin/gre-panel), else ./gre.sh (panel/ dev).
func greScriptPath() (string, error) {
	if p := os.Getenv("GRE_SCRIPT"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("GRE_SCRIPT=%s not found", p)
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "gre.sh"); fileExists(p) {
			return p, nil
		}
	}
	if fileExists("gre.sh") {
		if abs, err := filepath.Abs("gre.sh"); err == nil {
			return abs, nil
		}
		return "gre.sh", nil
	}
	return "", fmt.Errorf("gre.sh not found (set GRE_SCRIPT=/path/to/gre.sh)")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ---- GET /api/setup: defaults + whether a tunnel already exists ----

func handleSetupGet(w http.ResponseWriter, r *http.Request) {
	st := localStatus()
	writeJSON(w, map[string]any{
		"local_public": detectPublicIP(),
		"iran_gre":     defaultIranGRE,
		"foreign_gre":  defaultForeignGRE,
		"frp_port":     defaultFrpPort,
		"role_guess":   st.Role,
		"exists":       tunnelExists(),
	})
}

func tunnelExists() bool {
	if out, err := exec.Command("ip", "tunnel", "show").CombinedOutput(); err == nil {
		if strings.Contains(string(out), "gre-tunnel") {
			return true
		}
	}
	for _, f := range []string{"/etc/frp/frps.toml", "/etc/frp/frpc.toml"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

func detectPublicIP() string {
	out, err := exec.Command("ip", "route", "get", "1.1.1.1").CombinedOutput()
	if err == nil {
		f := strings.Fields(string(out))
		for i, p := range f {
			if p == "src" && i+1 < len(f) {
				if net.ParseIP(f[i+1]) != nil {
					return f[i+1]
				}
			}
		}
	}
	return ""
}

// ---- POST /api/setup ----

type setupRequest struct {
	Role      string `json:"role"` // "iran" | "foreign" | "add-peer"
	Name      string `json:"name"` // add-peer label
	LocalPub  string `json:"local_public"`
	RemotePub string `json:"remote_public"`
	LocalGre  string `json:"local_gre"`
	PeerGre   string `json:"peer_gre"`
	FrpPort   int    `json:"frp_port"`
	Token     string `json:"token"` // foreign/add-peer (manual or auto)
	Ports     string `json:"ports"` // foreign/add-peer, e.g. "443, 2083, 8080"
	Force     bool   `json:"force"`
	Autogen   bool   `json:"autogen"` // add-peer: generate token server-side
}

func handleSetupPost(w http.ResponseWriter, r *http.Request) {
	var body setupRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body.LocalPub = strings.TrimSpace(body.LocalPub)
	body.RemotePub = strings.TrimSpace(body.RemotePub)
	body.LocalGre = strings.TrimSpace(body.LocalGre)
	body.PeerGre = strings.TrimSpace(body.PeerGre)
	body.Token = strings.TrimSpace(body.Token)
	body.Name = strings.TrimSpace(body.Name)

	// add-peer mode: each new foreign server gets its own token + tunnel.
	// Port conflicts (same remotePort on two peers) are rejected with 409
	// so the user picks another port instead of silently breaking a peer.
	if body.Role == "add-peer" {
		if body.Autogen || body.Token == "" {
			body.Token = randomToken(16)
		}
		if len(loadPeers()) >= 5 {
			http.Error(w, "peer table full (max 5 foreign servers) — remove one first", http.StatusConflict)
			return
		}
	}

	// validation (mirrors gre.sh prompt_* / validate_setup_common rules)
	if body.Role != "iran" && body.Role != "foreign" && body.Role != "add-peer" {
		http.Error(w, "role must be iran, foreign or add-peer", http.StatusBadRequest)
		return
	}
	if net.ParseIP(body.LocalPub) == nil {
		http.Error(w, "invalid local public IP", http.StatusBadRequest)
		return
	}
	if net.ParseIP(body.RemotePub) == nil {
		http.Error(w, "invalid remote public IP", http.StatusBadRequest)
		return
	}
	if net.ParseIP(body.LocalGre) == nil || !isV4(body.LocalGre) {
		http.Error(w, "invalid local GRE IP", http.StatusBadRequest)
		return
	}
	if net.ParseIP(body.PeerGre) == nil || !isV4(body.PeerGre) {
		http.Error(w, "invalid peer GRE IP", http.StatusBadRequest)
		return
	}
	if body.FrpPort < 1 || body.FrpPort > 65535 {
		http.Error(w, "frp port must be 1-65535", http.StatusBadRequest)
		return
	}

	var ports []int
	if body.Role == "foreign" || body.Role == "add-peer" {
		if body.Token == "" {
			http.Error(w, "token from Iran side is required", http.StatusBadRequest)
			return
		}
		if len(body.Token) > 128 {
			http.Error(w, "token too long", http.StatusBadRequest)
			return
		}
		ports = parsePorts(body.Ports)
		if len(ports) == 0 {
			http.Error(w, "at least one reverse port is required (e.g. 443, 2083)", http.StatusBadRequest)
			return
		}
	}

	// add-peer pre-check: reject ports another tunnel already serves (409 + peer name)
	if body.Role == "add-peer" {
		if clash := peerPortClash(ports, -1); clash != "" {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]string{
				"error": "port " + clash + " is already served by another tunnel — pick a different port",
			})
			return
		}
	}

	// overwrite guard (per user decision: warn first, proceed only with force)
	// add-peer never overwrites: it appends a new tunnel instead.
	if body.Role != "add-peer" && tunnelExists() && !body.Force {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]string{
			"error": "tunnel already exists — resubmit with force:true to overwrite",
		})
		return
	}

	// run the shared installer: GRE_SKIP_PANEL=1 because the panel is already
	// running here — reinstalling/downloading it mid-request would be slow and
	// could restart this very process.
	token, steps, err := runInstaller(body, ports)
	if err != nil {
		steps = append(steps, "FAILED: "+err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"steps": steps})
		return
	}
	out := map[string]any{"status": "ok", "steps": steps}
	if token != "" {
		out["token"] = token
	}
	writeJSON(w, out)
}

// runInstaller shells out to gre.sh setup-iran|setup-foreign with the same
// flags the CLI uses, so menu / CLI / panel execute identical steps.
// Returns the generated Iran token ("", steps, nil) for foreign.
func runInstaller(b setupRequest, ports []int) (string, []string, error) {
	script, err := greScriptPath()
	if err != nil {
		return "", nil, err
	}
	token := ""
	args := []string{}
	if b.Role == "add-peer" {
		name := b.Name
		if name == "" {
			name = "peer"
		}
		strs := make([]string, len(ports))
		for i, q := range ports {
			strs[i] = strconv.Itoa(q)
		}
		token = b.Token
		args = []string{"add-peer",
			"--name", name,
			"--local-pub", b.LocalPub, "--remote-pub", b.RemotePub,
			"--frp-port", strconv.Itoa(b.FrpPort),
			"--local-gre", b.LocalGre, "--peer-gre", b.PeerGre,
			"--token", token,
			"--ports", strings.Join(strs, ","),
		}
	} else if b.Role == "iran" {
		token = randomToken(16)
		args = []string{"setup-iran",
			"--local-pub", b.LocalPub, "--remote-pub", b.RemotePub,
			"--frp-port", strconv.Itoa(b.FrpPort),
			"--local-gre", b.LocalGre, "--peer-gre", b.PeerGre,
			"--token", token,
		}
	} else {
		strs := make([]string, len(ports))
		for i, p := range ports {
			strs[i] = strconv.Itoa(p)
		}
		args = []string{"setup-foreign",
			"--local-pub", b.LocalPub, "--remote-pub", b.RemotePub,
			"--frp-port", strconv.Itoa(b.FrpPort),
			"--local-gre", b.LocalGre, "--peer-gre", b.PeerGre,
			"--token", b.Token,
			"--ports", strings.Join(strs, ","),
		}
	}
	if b.Force {
		args = append(args, "--force")
	}
	// token is used verbatim as an argv element (no shell), safe from injection.
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "GRE_SKIP_PANEL=1")
	out, runErr := cmd.CombinedOutput()
	steps := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			steps = append(steps, line)
		}
	}
	if b.Role == "iran" {
		steps = append([]string{"token generated (copy to Foreign side)"}, steps...)
	}
	if runErr != nil {
		return "", steps, fmt.Errorf("gre.sh %s failed: %w", args[0], runErr)
	}
	if b.Role == "iran" {
		return token, steps, nil
	}
	return "", steps, nil
}

// ---- peers API: list tunnels + token lookup ----

func handlePeersGet(w http.ResponseWriter, r *http.Request) {
	if idStr := r.URL.Query().Get("id"); idStr != "" {
		id, err := strconv.Atoi(idStr)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		script, err := greScriptPath()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cmd := exec.Command("bash", script, "peer-token", "--id", strconv.Itoa(id))
		cmd.Env = append(os.Environ(), "GRE_SKIP_PANEL=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			http.Error(w, strings.TrimSpace(string(out)), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]string{"id": idStr, "token": strings.TrimSpace(string(out))})
		return
	}
	peers := livePeers()
	if peers == nil {
		peers = []peerLive{}
	}
	writeJSON(w, map[string]any{"peers": peers, "peer_count": len(peers), "max": 5})
}

// POST /api/peers just proxies validation errors from /api/setup role=add-peer.
// The real creation path is /api/setup (role add-peer) so only one code path
// shells out to gre.sh. This endpoint exists for future per-peer edits.
func handlePeersPost(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "use POST /api/setup with role=add-peer", http.StatusBadRequest)
}

func isV4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

// peerPortClash reports "PORT (peer NAME)" for the first requested port that
// another registered tunnel already serves. excludeID skips one peer (<0 = none).
func peerPortClash(want []int, excludeID int) string {
	claimed := map[int]string{}
	for _, p := range loadPeers() {
		if p.ID == excludeID {
			continue
		}
		for _, port := range p.Ports {
			claimed[port] = p.Name
		}
	}
	for _, port := range want {
		if name, ok := claimed[port]; ok {
			return fmt.Sprintf("%d (peer %s)", port, name)
		}
	}
	return ""
}

func parsePorts(s string) []int {
	var out []int
	seen := map[int]bool{}
	for _, p := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > 65535 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func randomToken(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("token-%d", n)
	}
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}
