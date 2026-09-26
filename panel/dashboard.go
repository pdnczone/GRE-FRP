package main

// Dashboard API: one JSON snapshot for the Dashboard tab.
// Every value is read live from the system; anything unavailable is null
// and the frontend renders it as N/A (never crashes).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var panelStartedAt = time.Now()

// last CPU sample for delta-based usage %. First request returns N/A.
var (
	cpuMu   sync.Mutex
	cpuPrev cpuSample
	cpuHave bool
)

type cpuSample struct {
	idle  uint64
	total uint64
	when  time.Time
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	st := localStatus()
	traffic := greTraffic()
	recordTrafficSample(traffic)
	d := map[string]any{
		"online":     st.Gre.Exists || st.FrpUp,
		"ping_ok":    st.PingOK,
		"ping":      nilIfEmpty(st.PingMs),
		"role":      nilIfEmpty(st.Role),
		"local_pub": nilIfEmpty(detectPublicIP()),
		"gre": map[string]any{
			"exists": st.Gre.Exists,
			"inner":  nilIfEmpty(st.Gre.Inner),
			"local":  nilIfEmpty(st.Gre.Local),
			"peer":   nilIfEmpty(peerOr(st)),
		},
		"frp": map[string]any{
			"up":      st.FrpUp,
			"svc":     nilIfEmpty(st.FrpSvc),
			"port":    nilIfZero(st.FrpPort),
			"proxies": st.Proxies,
		},
		"traffic": traffic,
		"history": trafficHistory(r.URL.Query().Get("range")),
		"uptime":  uptimeInfo(st),
		"system":  systemInfo(),
		"conns":   activeConns(),
		"checked": time.Now().UTC().Format(time.RFC3339),
	}
	writeJSON(w, d)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func peerOr(st tunnelStatus) string {
	if st.Gre.PeerIP != "" {
		return st.Gre.PeerIP
	}
	return st.GrePeer
}

// ---- traffic: rx/tx bytes of gre-tunnel from /proc/net/dev ----

func greTraffic() map[string]any {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return map[string]any{"up": nil, "down": nil, "total": nil}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "gre-tunnel:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "gre-tunnel:"))
		if len(fields) < 9 {
			break
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			break
		}
		return map[string]any{"up": tx, "down": rx, "total": tx + rx}
	}
	return map[string]any{"up": nil, "down": nil, "total": nil}
}

// ---- uptime: system + panel + frp service ----

func uptimeInfo(st tunnelStatus) map[string]any {
	out := map[string]any{"system": nil, "panel": nil, "frp_since": nil}
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		if secs, err := strconv.ParseFloat(strings.Fields(string(data))[0], 64); err == nil {
			out["system"] = int64(secs)
		}
	}
	out["panel"] = int64(time.Since(panelStartedAt).Seconds())
	if st.FrpSvc != "" {
		if ts, err := exec.Command("systemctl", "show", st.FrpSvc,
			"-p", "ActiveEnterTimestamp", "--value").CombinedOutput(); err == nil {
			if s := strings.TrimSpace(string(ts)); s != "" && s != "n/a" {
				out["frp_since"] = s
			}
		}
	}
	return out
}

// ---- system: cpu %, mem, disk, load ----

func systemInfo() map[string]any {
	return map[string]any{
		"cpu_pct":  cpuPct(),
		"load":     loadAvg(),
		"cores":    runtime.NumCPU(),
		"mem":      memInfo(),
		"disk_pct": diskPct("/"),
	}
}

func loadAvg() any {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return nil
	}
	return strings.Join(f[:3], " ")
}

func memInfo() map[string]any {
	out := map[string]any{"used": nil, "total": nil, "pct": nil}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return out
	}
	var total, avail uint64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	if total == 0 {
		return out
	}
	used := total - avail
	out["used"] = used
	out["total"] = total
	out["pct"] = fmt.Sprintf("%.1f", float64(used)*100/float64(total))
	return out
}

func diskPct(path string) any {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return nil
	}
	used := total - free
	return fmt.Sprintf("%.1f", float64(used)*100/float64(total))
}

// cpuPct reads /proc/stat and compares with the previous sample.
func cpuPct() any {
	cur, ok := readCPU()
	if !ok {
		return nil
	}
	cpuMu.Lock()
	defer cpuMu.Unlock()
	if !cpuHave {
		cpuPrev, cpuHave = cur, true
		return nil
	}
	dTotal := cur.total - cpuPrev.total
	dIdle := cur.idle - cpuPrev.idle
	cpuPrev = cur
	if dTotal == 0 {
		return nil
	}
	return fmt.Sprintf("%.1f", float64(dTotal-dIdle)*100/float64(dTotal))
}

func readCPU() (cpuSample, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)[1:]
		if len(f) < 5 {
			return cpuSample{}, false
		}
		nums := make([]uint64, len(f))
		for i, s := range f {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return cpuSample{}, false
			}
			nums[i] = v
		}
		var total uint64
		for _, v := range nums {
			total += v
		}
		idle := nums[3]
		if len(nums) > 4 {
			idle += nums[4] // iowait counts as idle
		}
		return cpuSample{idle: idle, total: total, when: time.Now()}, true
	}
	return cpuSample{}, false
}

// ---- connections: established TCP/UDP via ss ----

func activeConns() any {
	out, err := exec.Command("ss", "-tun", "state", "established").CombinedOutput()
	if err != nil {
		return nil
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Netid") {
			continue
		}
		n++
	}
	// ss always prints a header; n counts real connections.
	return n
}

// ---- traffic history: cumulative rx/tx sampled into a ring on disk ----

type trafficPoint struct {
	T     int64  `json:"t"`
	Up    *uint64 `json:"up"`
	Down  *uint64 `json:"down"`
	Total *uint64 `json:"total"`
}

var (
	histMu     sync.Mutex
	histCached []trafficPoint
	histLoaded bool
)

func historyFile() string { return filepath.Join(configDir, "traffic.json") }

func loadHistory() []trafficPoint {
	if histLoaded {
		return histCached
	}
	histLoaded = true
	data, err := os.ReadFile(historyFile())
	if err == nil {
		_ = json.Unmarshal(data, &histCached)
	}
	return histCached
}

func saveHistoryLocked() {
	_ = os.WriteFile(historyFile(), mustJSON(histCached), 0600)
}

// recordTrafficSample appends one point per dashboard poll (5s). Points are
// cumulative counters, so rate = delta between neighbours. Cap 90 days.
func recordTrafficSample(traffic map[string]any) {
	histMu.Lock()
	defer histMu.Unlock()
	hist := loadHistory()
	now := time.Now().Unix()
	if n := len(hist); n > 0 && now-hist[n-1].T < 4 {
		return // same poll, don't double-record
	}
	pt := trafficPoint{T: now}
	if v, ok := traffic["up"].(uint64); ok {
		c := v
		pt.Up = &c
	}
	if v, ok := traffic["down"].(uint64); ok {
		c := v
		pt.Down = &c
	}
	if v, ok := traffic["total"].(uint64); ok {
		c := v
		pt.Total = &c
	}
	hist = append(hist, pt)
	cutoff := now - 90*86400
	i := 0
	for i < len(hist) && hist[i].T < cutoff {
		i++
	}
	if i > 0 {
		hist = append([]trafficPoint(nil), hist[i:]...)
	}
	histCached = hist
	saveHistoryLocked()
}

// trafficHistory returns downsampled points for range=24h|7d|30d|90d.
// Default 24h. Missing interface (tunnel down) yields gaps: null values.
func trafficHistory(rng string) []trafficPoint {
	histMu.Lock()
	defer histMu.Unlock()
	hist := loadHistory()
	now := time.Now().Unix()
	span := int64(24 * 3600)
	switch rng {
	case "7d":
		span = 7 * 86400
	case "30d":
		span = 30 * 86400
	case "90d":
		span = 90 * 86400
	}
	cutoff := now - span
	var in []trafficPoint
	for _, p := range hist {
		if p.T >= cutoff {
			in = append(in, p)
		}
	}
	maxPts := 240
	if len(in) <= maxPts {
		if in == nil {
			return []trafficPoint{}
		}
		return in
	}
	step := float64(len(in)) / float64(maxPts)
	out := make([]trafficPoint, 0, maxPts)
	for i := 0; i < maxPts; i++ {
		out = append(out, in[int(float64(i)*step)])
	}
	return out
}
