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
	"strconv"
	"strings"
	"time"
)

// status of GRE + FRP on this machine.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	st := localStatus()
	peers := livePeers()
	writeJSON(w, map[string]any{"local": st, "peers": peers, "peer_count": len(peers)})
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("svc")
	n := r.URL.Query().Get("n")
	lines := 100
	if v, err := strconv.Atoi(n); err == nil && v >= 10 && v <= 1000 {
		lines = v
	}
	// allowed units: legacy frps/frpc, per-peer frps-N, GRE units, panel itself
	allowed := map[string]bool{"frps": true, "frpc": true, "gre-panel": true,
		"gre-tunnel": true, "gre-tunnel.service": true}
	if !allowed[svc] {
		if strings.HasPrefix(svc, "frps-") || strings.HasPrefix(svc, "gre-t") {
			allowed[svc] = true
		}
	}
	if !allowed[svc] {
		// unknown? fall back to whichever FRP side exists
		svc = "frps"
		if _, err := os.Stat("/etc/frp/frpc.toml"); err == nil {
			svc = "frpc"
		}
	}
	unit := svc
	if !strings.HasSuffix(unit, ".service") && unit != "gre-panel" {
		// journalctl accepts short names for frps/frpc too, keep as-is
	}
	if _, err := exec.LookPath("journalctl"); err == nil {
		out, err := exec.Command("journalctl", "-u", unit, "-n", strconv.Itoa(lines), "--no-pager").CombinedOutput()
		if err == nil {
			writeJSON(w, map[string]string{"logs": string(out), "svc": svc})
			return
		}
	}
	// fallback: log files
	for _, q := range []string{"/var/log/" + svc + ".log", "/root/" + svc + ".log"} {
		if data, err := os.ReadFile(q); err == nil {
			writeJSON(w, map[string]string{"logs": string(data), "svc": svc})
			return
		}
	}
	writeJSON(w, map[string]string{"logs": "(no logs available — is " + svc + " installed?)", "svc": svc})
}

// actions: restart frps/frpc/gre, ping peer, optimize/restore network tuning.
func handleAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
		PeerID int    `json:"peer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch body.Action {
	case "restart-frps", "restart-frpc", "restart-gre", "ping", "remove-tunnel":
		out, err := runAction(body.Action, body.PeerID)
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

func runAction(action string, peerID int) (string, error) {
	switch action {
	case "restart-frps":
		svc := peerFrpsSvc(peerID)
		out, err := exec.Command("systemctl", "restart", svc).CombinedOutput()
		return string(out), err
	case "restart-frpc":
		out, err := exec.Command("systemctl", "restart", "frpc").CombinedOutput()
		return string(out), err
	case "restart-gre":
		svc := peerGreSvc(peerID)
		out, err := exec.Command("systemctl", "restart", svc).CombinedOutput()
		return string(out), err
	case "ping":
		target := peerPingTarget(peerID)
		if target == "" {
			return "", fmt.Errorf("no GRE peer known")
		}
		out, err := exec.Command("ping", "-c", "3", "-W", "2", target).CombinedOutput()
		return string(out), err
	case "remove-tunnel":
		if peerID > 0 {
			return removePeerViaInstaller(peerID)
		}
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

// ---- multi-peer: registry + per-tunnel inspection ----
// peers.json (written by gre.sh add-peer) is the source of truth for how
// many foreign servers hang off this Iran. Legacy single installs without
// a registry fall back to the old single-tunnel view.

type peerRecord struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	LocalPub string `json:"local_pub"`
	RemotePub string `json:"remote_pub"`
	FrpPort  int    `json:"frp_port"`
	LocalGre string `json:"local_gre"`
	PeerGre  string `json:"peer_gre"`
	Ports    []int  `json:"ports"`
	GreIf    string `json:"gre_if"`
	FrpsSvc  string `json:"frps_svc"`
}

type peerLive struct {
	peerRecord
	GreUp    bool   `json:"gre_up"`
	GreInner string `json:"gre_inner"`
	FrpUp    bool   `json:"frp_up"`
	PingOK   bool   `json:"ping_ok"`
	PingMs   string `json:"ping_ms"`
	Rx       *uint64 `json:"rx"`
	Tx       *uint64 `json:"tx"`
}

func peersFile() string { return configDir + "/peers.json" }

func loadPeers() []peerRecord {
	data, err := os.ReadFile(peersFile())
	if err != nil {
		return nil
	}
	var v struct {
		Peers []peerRecord `json:"peers"`
	}
	if json.Unmarshal(data, &v) != nil {
		return nil
	}
	return v.Peers
}

func findPeer(id int) *peerRecord {
	for _, p := range loadPeers() {
		if p.ID == id {
			c := p
			return &c
		}
	}
	return nil
}

// unit names for actions; peerID 0 = legacy default.
func peerFrpsSvc(peerID int) string {
	if peerID > 0 {
		if p := findPeer(peerID); p != nil && p.FrpsSvc != "" {
			return p.FrpsSvc
		}
		if peerID > 1 {
			return fmt.Sprintf("frps-%d", peerID)
		}
	}
	return "frps"
}

func peerGreSvc(peerID int) string {
	if peerID > 0 {
		if p := findPeer(peerID); p != nil && p.GreIf != "" {
			return p.GreIf + ".service"
		}
		if peerID > 1 {
			return fmt.Sprintf("gre-t%d.service", peerID)
		}
	}
	return "gre-tunnel.service"
}

func peerPingTarget(peerID int) string {
	if peerID > 0 {
		if p := findPeer(peerID); p != nil && p.PeerGre != "" {
			return p.PeerGre
		}
	}
	return localStatus().GrePeer
}

func ifaceTraffic(ifname string) (rx, tx *uint64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, ifname+":") {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(line, ifname+":"))
		if len(f) < 9 {
			return nil, nil
		}
		var r, t uint64
		if _, err := fmt.Sscanf(f[0], "%d", &r); err != nil {
			return nil, nil
		}
		if _, err := fmt.Sscanf(f[8], "%d", &t); err != nil {
			return nil, nil
		}
		return &r, &t
	}
	return nil, nil
}

func ifaceInner(ifname string) string {
	out, err := exec.Command("ip", "-4", "addr", "show", "dev", ifname).CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "inet ") {
			return strings.Fields(line)[1]
		}
	}
	return ""
}

func svcActive(svc string) bool {
	out, err := exec.Command("systemctl", "is-active", svc).CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// livePeers inspects every registered peer: GRE up, frps up, ping, counters.
func livePeers() []peerLive {
	recs := loadPeers()
	if len(recs) == 0 {
		return nil
	}
	out := make([]peerLive, 0, len(recs))
	for _, p := range recs {
		l := peerLive{peerRecord: p}
		if _, err := exec.Command("ip", "tunnel", "show").CombinedOutput(); err == nil {
			// presence check via interface address (works without parsing tun show)
			l.GreInner = ifaceInner(p.GreIf)
			l.GreUp = l.GreInner != ""
		}
		l.FrpUp = svcActive(p.FrpsSvc)
		if p.PeerGre != "" {
			start := time.Now()
			if err := exec.Command("ping", "-c", "1", "-W", "2", p.PeerGre).Run(); err == nil {
				l.PingOK = true
				l.PingMs = fmt.Sprintf("%.0fms", float64(time.Since(start).Microseconds())/1000)
			}
		}
		l.Rx, l.Tx = ifaceTraffic(p.GreIf)
		out = append(out, l)
	}
	return out
}

func removePeerViaInstaller(id int) (string, error) {
	script, err := greScriptPath()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("bash", script, "remove-peer", "--id", fmt.Sprint(id), "--force")
	cmd.Env = append(os.Environ(), "GRE_SKIP_PANEL=1")
	out, runErr := cmd.CombinedOutput()
	o := strings.TrimSpace(string(out))
	if o == "" {
		o = fmt.Sprintf("peer %d removed", id)
	}
	if runErr != nil {
		return o, fmt.Errorf("remove-peer failed: %w", runErr)
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
