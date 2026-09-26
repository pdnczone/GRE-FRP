package main

// Setup runners: mirror gre.sh steps (GRE systemd + frps/frpc configs).

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const (
	frpInstallDir = "/usr/local/bin"
	frpConfigDir  = "/etc/frp"
)

func sh(step string, steps *[]string, name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	*steps = append(*steps, fmt.Sprintf("$ %s %s → %s", name,
		strings.Join(args, " "), strings.TrimSpace(string(out))))
	if err != nil {
		return fmt.Errorf("%s: %s: %w", step, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func runIranSetup(b setupRequest) (string, []string, error) {
	var steps []string
	token := randomToken(16)
	steps = append(steps, "token generated (copy to Foreign side)")

	if err := writeGreService(b.LocalPub, b.RemotePub, b.LocalGre, &steps); err != nil {
		return "", steps, err
	}
	if err := ensureFRP(&steps); err != nil {
		return "", steps, err
	}
	cfg := fmt.Sprintf("bindAddr = \"0.0.0.0\"\nbindPort = %d\nauth.method = \"token\"\nauth.token = %q\ntransport.tls.force = false\n",
		b.FrpPort, token)
	if err := os.MkdirAll(frpConfigDir, 0755); err != nil {
		return "", steps, err
	}
	if err := os.WriteFile(frpConfigDir+"/frps.toml", []byte(cfg), 0600); err != nil {
		return "", steps, fmt.Errorf("write frps.toml: %w", err)
	}
	steps = append(steps, fmt.Sprintf("frps.toml written (bindPort %d)", b.FrpPort))

	svc := "[Unit]\nDescription=FRP Server Service\nAfter=network.target gre-tunnel.service\nWants=gre-tunnel.service\n\n[Service]\nType=simple\nUser=root\nRestart=always\nRestartSec=5s\nExecStart=" + frpInstallDir + "/frps -c " + frpConfigDir + "/frps.toml\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile("/etc/systemd/system/frps.service", []byte(svc), 0644); err != nil {
		return "", steps, fmt.Errorf("write frps.service: %w", err)
	}
	for _, a := range [][]string{
		{"daemon-reload"}, {"enable", "frps"}, {"restart", "frps"},
	} {
		if err := sh("frps", &steps, "systemctl", a...); err != nil {
			return "", steps, err
		}
	}
	if _, err := exec.LookPath("ufw"); err == nil {
		if out, _ := exec.Command("ufw", "status").CombinedOutput(); strings.Contains(string(out), "Status: active") {
			_ = sh("ufw", &steps, "ufw", "allow", fmt.Sprintf("%d/tcp", b.FrpPort))
		}
	}
	steps = append(steps, fmt.Sprintf("DONE: GRE %s ↔ %s, frps :%d", b.LocalPub, b.RemotePub, b.FrpPort))
	return token, steps, nil
}

func runForeignSetup(b setupRequest, ports []int) ([]string, error) {
	var steps []string
	if err := writeGreService(b.LocalPub, b.RemotePub, b.LocalGre, &steps); err != nil {
		return steps, err
	}
	// auto ping after GRE (per user decision)
	out, err := exec.Command("ping", "-c", "3", "-W", "2", b.PeerGre).CombinedOutput()
	steps = append(steps, fmt.Sprintf("$ ping -c 3 %s → %s", b.PeerGre, strings.TrimSpace(string(out))))
	if err != nil {
		steps = append(steps, "WARNING: GRE ping no answer yet (configure Iran side first?)")
	} else {
		steps = append(steps, "GRE ping OK")
	}
	if err := ensureFRP(&steps); err != nil {
		return steps, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "serverAddr = %q\nserverPort = %d\nauth.method = \"token\"\nauth.token = %q\ntransport.tls.enable = true\n\n",
		b.PeerGre, b.FrpPort, b.Token)
	for _, p := range ports {
		fmt.Fprintf(&sb, "[[proxies]]\nname = \"tcp_%d\"\ntype = \"tcp\"\nlocalIP = \"127.0.0.1\"\nlocalPort = %d\nremotePort = %d\n\n", p, p, p)
		fmt.Fprintf(&sb, "[[proxies]]\nname = \"udp_%d\"\ntype = \"udp\"\nlocalIP = \"127.0.0.1\"\nlocalPort = %d\nremotePort = %d\n\n", p, p, p)
	}
	if err := os.MkdirAll(frpConfigDir, 0755); err != nil {
		return steps, err
	}
	if err := os.WriteFile(frpConfigDir+"/frpc.toml", []byte(sb.String()), 0600); err != nil {
		return steps, fmt.Errorf("write frpc.toml: %w", err)
	}
	steps = append(steps, fmt.Sprintf("frpc.toml written (%d ports, TCP+UDP, TLS)", len(ports)))

	svc := "[Unit]\nDescription=FRP Client Reverse Service\nAfter=network.target gre-tunnel.service\nWants=gre-tunnel.service\n\n[Service]\nType=simple\nUser=root\nRestart=always\nRestartSec=5s\nExecStart=" + frpInstallDir + "/frpc -c " + frpConfigDir + "/frpc.toml\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile("/etc/systemd/system/frpc.service", []byte(svc), 0644); err != nil {
		return steps, fmt.Errorf("write frpc.service: %w", err)
	}
	for _, a := range [][]string{
		{"daemon-reload"}, {"enable", "frpc"}, {"restart", "frpc"},
	} {
		if err := sh("frpc", &steps, "systemctl", a...); err != nil {
			return steps, err
		}
	}
	steps = append(steps, fmt.Sprintf("DONE: GRE %s ↔ %s, frpc → %s:%d",
		b.LocalPub, b.RemotePub, b.PeerGre, b.FrpPort))
	return steps, nil
}

func writeGreService(localPub, remotePub, inner string, steps *[]string) error {
	_ = exec.Command("ip", "tunnel", "del", "gre-tunnel").Run()
	svc := "[Unit]\nDescription=GRE Tunnel Interface\nAfter=network.target\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStartPre=-/sbin/ip tunnel del gre-tunnel\nExecStart=/bin/sh -c \"/sbin/ip tunnel add gre-tunnel mode gre local " +
		localPub + " remote " + remotePub +
		" ttl 255 && /sbin/ip link set dev gre-tunnel up mtu 1476 && /sbin/ip addr add " +
		inner + "/30 dev gre-tunnel\"\nExecStop=-/sbin/ip tunnel del gre-tunnel\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile("/etc/systemd/system/gre-tunnel.service", []byte(svc), 0644); err != nil {
		return fmt.Errorf("write gre service: %w", err)
	}
	*steps = append(*steps, fmt.Sprintf("GRE service written (%s ↔ %s, %s/30)", localPub, remotePub, inner))
	for _, a := range [][]string{
		{"daemon-reload"}, {"enable", "gre-tunnel.service"}, {"restart", "gre-tunnel.service"},
	} {
		if err := sh("gre", steps, "systemctl", a...); err != nil {
			return err
		}
	}
	_ = exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run()
	_ = exec.Command("iptables", "-t", "mangle", "-C", "POSTROUTING",
		"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN",
		"-j", "TCPMSS", "--clamp-mss-to-pmtu").Run()
	if _, err := exec.LookPath("iptables"); err == nil {
		out, _ := exec.Command("iptables", "-t", "mangle", "-C", "POSTROUTING",
			"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN",
			"-j", "TCPMSS", "--clamp-mss-to-pmtu").CombinedOutput()
		if len(out) > 0 {
			_ = exec.Command("iptables", "-t", "mangle", "-A", "POSTROUTING",
				"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN",
				"-j", "TCPMSS", "--clamp-mss-to-pmtu").Run()
		}
	}
	*steps = append(*steps, "GRE up (mtu 1476, forwarding + MSS clamp)")
	return nil
}

func ensureFRP(steps *[]string) error {
	for _, b := range []string{"frps", "frpc"} {
		if _, err := exec.LookPath(b); err == nil {
			continue
		}
		if err := installOneFRP(b, steps); err != nil {
			return err
		}
	}
	*steps = append(*steps, "frps/frpc binaries present")
	return nil
}

func installOneFRP(bin string, steps *[]string) error {
	arch := runtime.GOARCH
	var fa string
	switch arch {
	case "amd64":
		fa = "amd64"
	case "arm64":
		fa = "arm64"
	case "arm":
		fa = "arm"
	default:
		return fmt.Errorf("unsupported arch %s", arch)
	}
	tmp, err := os.MkdirTemp("", "frpdl")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	tar := fmt.Sprintf("frp_%s_linux_%s.tar.gz", frpVersion, fa)
	url := fmt.Sprintf("https://github.com/fatedier/frp/releases/download/v%s/%s", frpVersion, tar)
	*steps = append(*steps, fmt.Sprintf("downloading FRP v%s (%s)...", frpVersion, fa))
	if out, err := exec.Command("curl", "-sSL", "-o", tmp+"/"+tar, url).CombinedOutput(); err != nil {
		return fmt.Errorf("download frp: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if out, err := exec.Command("tar", "-xzf", tmp+"/"+tar, "-C", tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("extract frp: %s: %w", strings.TrimSpace(string(out)), err)
	}
	src := fmt.Sprintf("%s/frp_%s_linux_%s/%s", tmp, frpVersion, fa, bin)
	if out, err := exec.Command("cp", src, frpInstallDir+"/").CombinedOutput(); err != nil {
		return fmt.Errorf("install %s: %s: %w", bin, strings.TrimSpace(string(out)), err)
	}
	if out, err := exec.Command("chmod", "+x", frpInstallDir+"/"+bin).CombinedOutput(); err != nil {
		return fmt.Errorf("chmod %s: %s: %w", bin, strings.TrimSpace(string(out)), err)
	}
	*steps = append(*steps, fmt.Sprintf("%s installed to %s", bin, frpInstallDir))
	return nil
}
