package main

// Setup API: build the GRE+FRP tunnel from the web panel.
// Each panel configures ONLY its own side (per user decision).
// Iran side: GRE + frps (token auto-generated, shown for copy to Turkey).
// Foreign side: GRE + frpc (token entered manually, ports list like "443, 2083").

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	defaultIranGRE    = "10.10.10.2"
	defaultForeignGRE = "10.10.10.1"
	defaultFrpPort    = 7000
	frpVersion        = "0.71.0"
)

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
	Role       string `json:"role"` // "iran" | "foreign"
	LocalPub   string `json:"local_public"`
	RemotePub  string `json:"remote_public"`
	LocalGre   string `json:"local_gre"`
	PeerGre    string `json:"peer_gre"`
	FrpPort    int    `json:"frp_port"`
	Token      string `json:"token"` // foreign only (manual); iran ignores
	Ports      string `json:"ports"` // foreign only, e.g. "443, 2083, 8080"
	Force      bool   `json:"force"`
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

	// validation
	if body.Role != "iran" && body.Role != "foreign" {
		http.Error(w, "role must be iran or foreign", http.StatusBadRequest)
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
	if body.Role == "foreign" {
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

	// overwrite guard (per user decision: warn first, proceed only with force)
	if tunnelExists() && !body.Force {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]string{
			"error": "tunnel already exists — resubmit with force:true to overwrite",
		})
		return
	}

	var steps []string
	var token string
	var err error
	if body.Role == "iran" {
		token, steps, err = runIranSetup(body)
	} else {
		steps, err = runForeignSetup(body, ports)
	}
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

func isV4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
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
