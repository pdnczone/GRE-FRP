package main

// Tunnel inspection + actions: read GRE/FRP state via ip/systemd (never
// writes), restart/ping/remove via systemctl/ping or the shared gre.sh
// installer (single source of truth, same as the CLI/menu path).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// status of GRE + FRP on this machine.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"local": localStatus()})
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

// actions: restart frps/frpc/gre, ping peer, optimize/restore network tuning.
func handleAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch body.Action {
	case "restart-frps", "restart-frpc", "restart-gre", "ping", "remove-tunnel":
		out, err := runAction(body.Action)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"status": "ok", "output": out})
	case "optimize", "restore", "tune-status":
		// network tuning via the installer (single source of truth).
		out, err := tuneViaInstaller(body.Action)
		if err != nil {
			http.Error(w, out+": "+err.Error(), http.StatusBadRequest)
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
	case "remove-tunnel":
		// mirror of gre.sh remove_tunnel_force(): GRE + FRP gone, panel untouched.
		// Implemented via the installer itself (single source of truth) so the
		// shell-out path and the menu path can never drift apart.
		out, err := removeViaInstaller()
		if err != nil {
			return "", err
		}
		return out, nil
	}
	return "", fmt.Errorf("unknown action")
}

// tuneViaInstaller runs `gre.sh optimize|restore|tune-status` and returns its
// output as the action result (single source of truth, same as menu/CLI).
func tuneViaInstaller(action string) (string, error) {
	script, err := greScriptPath()
	if err != nil {
		return "", err
	}
	arg := map[string]string{
		"optimize": "optimize", "restore": "restore", "tune-status": "tune-status",
	}[action]
	cmd := exec.Command("bash", script, arg)
	cmd.Env = append(os.Environ(), "GRE_SKIP_PANEL=1")
	out, runErr := cmd.CombinedOutput()
	o := strings.TrimSpace(string(out))
	if o == "" {
		o = action + " done"
	}
	if runErr != nil {
		return o, fmt.Errorf("tune command failed: %w", runErr)
	}
	return o, nil
}

// removeViaInstaller runs `gre.sh remove-tunnel --force` and returns its
// output as the action result. GRE_SKIP_PANEL is irrelevant here (removal
// never touches the panel), but kept for symmetry with runInstaller.
func removeViaInstaller() (string, error) {
	script, err := greScriptPath()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("bash", script, "remove-tunnel", "--force")
	cmd.Env = append(os.Environ(), "GRE_SKIP_PANEL=1")
	out, runErr := cmd.CombinedOutput()
	o := strings.TrimSpace(string(out))
	if o == "" {
		o = "tunnel removed — panel still running"
	}
	if runErr != nil {
		return o, fmt.Errorf("remove-tunnel failed: %w", runErr)
	}
	return o, nil
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
