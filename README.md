# GRE + FRP Reverse Tunnel 🇮🇷 ↔ 🌍

[![Latest Release](https://img.shields.io/github/release/pdnczone/hashem?display_name=tag)](https://github.com/pdnczone/hashem/releases/latest)
[![Build Panel](https://github.com/pdnczone/hashem/actions/workflows/build-panel.yml/badge.svg)](https://github.com/pdnczone/hashem/actions/workflows/build-panel.yml)
[![Platform](https://img.shields.io/badge/platform-linux%20amd64%20%7C%20arm64-blue)](https://github.com/pdnczone/hashem)

Layer-3 GRE tunnel + encrypted TLS FRP reverse relay. Iran's IP stays behind the tunnel; foreign-server ports become reachable through Iran's public IP.

> **خلاصه فارسی:** تونل لایه ۳ GRE + ریورس TLS از FRP. آی‌پی ایران پشت تونل می‌مونه و پورت‌های سرور خارج از طریق آی‌پی ایران در دسترس قرار می‌گیرن. نصب با یک خط (پایین)، بعد گزینه `1` روی ایران و گزینه `2` روی سرور خارج. پنل وب خودکار نصب می‌شه و آخر نصب لینک + یوزر + پسورد رو نشون می‌ده.

---

## Architecture

```
Users ──► IRAN (public ports here) ══ GRE + FRP ══► FOREIGN (service runs here)
          frps listens :7000/:443…              frpc dials via 10.10.10.2
          GRE 10.10.10.2/30                     GRE 10.10.10.1/30
```

| Part | Iran server | Foreign server |
|------|-------------|----------------|
| GRE | `10.10.10.2/30` (`gre-tunnel`, systemd) | `10.10.10.1/30` (`gre-tunnel`, systemd) |
| FRP | `frps` (server, TLS) | `frpc` (client, connects to `10.10.10.2` **inside** the tunnel) |
| Services | `systemd`, auto-start on boot | `systemd`, auto-start on boot |
| Web panel | `gre-panel` on `:7777/<secret>` | `gre-panel` on `:7777/<secret>` (its own side only) |

- Auto arch detect (`amd64` / `arm64`), FRP download, `ip_forward` + TCPMSS clamp
- Every port = `tcp` + `udp` proxy with the same number on Iran
- Panel binary is **prebuilt on GitHub** (`panel-rN` releases) — no Go needed on servers

---

## Quick install

One-liner (both servers):

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/pdnczone/hashem/main/install.sh)
```

Manual:

```bash
git clone https://github.com/pdnczone/hashem.git
cd Hashem
sudo bash gre.sh
```

### Setup flow

1. **Iran first:** option `1` — give Iran + foreign public IPs, FRP port (default `7000`), keep the **token**.
2. **Foreign next:** option `2` — Iran IP, port, token from step 1 + port list to reverse (e.g. `443, 2083, 8080`).
3. **Check:** option `3` — GRE status, inner ping, FRP service state.
4. Panel installs automatically with setup; credentials print at the end (also option `8` anytime).

> **فارسی:** اول روی ایران گزینه `1` (آی‌پی‌ها + پورت + توکن رو نگه دار)، بعد روی خارج گزینه `2` (توکن + پورت‌ها). تست با گزینه `3`. پنل وب خودکار نصب می‌شه.

---

## Web panel

Each panel manages **only its own side** (no remote control). After setup it auto-installs and prints:

```
Panel URL:  http://<server-ip>:7777/<secret-path>
Username:   admin
Password:   8-digit number
```

Tabs: **Tunnel** (status + ping/restart) · **Setup** (Quick Setup 1-2-3, overwrite-guarded) · **Logs** (frps/frpc) · **Settings** (change password). Dark/light switch in the sidebar.

After login you can also re-run the tunnel setup from the browser — same logic as the script, step-by-step log included.

`grepanel` CLI (on the server):

```bash
grepanel status | logs | restart | url | password | uninstall
```

---

## Menu reference

| Option | What it does |
|--------|--------------|
| 1 | Setup IRAN (GRE + `frps`) + auto-install panel |
| 2 | Setup FOREIGN (GRE + `frpc` reverse) + auto-install panel |
| 3 | Status + GRE ping test |
| 4 | Live FRP logs |
| 5 | Restart tunnel services |
| 6 | Uninstall everything (services + interface + binaries) |
| 7 | Update all (latest script + latest prebuilt panel) |
| 8 | Show panel URL + username + password |
| 9 | Remove tunnel (GRE + FRP gone, **panel stays**) |
| 0 | Exit |

---

## Troubleshooting

| Symptom | Likely cause → fix |
|---------|-------------------|
| `Panel URL` empty / 404 | Secret path rotated after reinstall → option `8` prints the current one |
| Panel slow on Iran, fast on foreign | Client→Iran route (VPN/filtering), not the panel — try with/without VPN, compare ping |
| `port unavailable` (e.g. 8080) | Something already listens there (`ss -tlnp \| grep 8080`) — pick another port or stop it |
| FRP up but service unreachable | Check `4` (logs), `3` (GRE ping), token match on both sides, GRE proto 47 open between servers |
| Old binary after option `7` | GitHub `latest` redirect cache — re-run `7` once more; it verifies ELF before installing |

Requirements: Linux + `systemd`, `root`, GRE (protocol 47) open between servers.

---

## Security note

⚠️ The panel password is also saved in plaintext at `/etc/gre-panel/panel.pass` (mode `600`) so option `8` can show it — convenient, not maximally secure. Anyone with root on the server can read it.

- Change it anytime: panel **Settings** tab, or `grepanel password`, or option `8` auto-regenerates if missing.
- The panel listens on `:7777` — restrict with firewall to your IP if exposed.

> **فارسی:** پسورد پنل به‌صورت متنی در `panel.pass` ذخیره می‌شه تا گزینه `8` نشونش بده (انتخاب آگاهانه برای راحتی). هر وقت خواستی از تب Settings عوضش کن و پورت `7777` رو با فایروال محدود کن.

---

## `spoof_test.py` — direct vs tunneled spoof test

Single-file Python, no dependencies (raw socket needs `root`). Tested on this pair: direct spoof dropped by ingress filtering, tunneled mode seen on `lo`.

```bash
# 1. direct: packet with forged source straight to target
sudo python3 spoof_test.py direct --target 85.198.48.162 --port 55999 --spoof-src 192.0.2.1
# on target watch: tcpdump -n 'udp port 55999'

# 2. on destination: helper (opens + injects locally)
sudo python3 spoof_test.py helper --listen-port 55996 --deliver-port 55999

# 3. from source: tunnel (hands forged packet to helper)
sudo python3 spoof_test.py tunnel --helper 85.198.48.162 --helper-port 55996 \
    --spoof-src 192.0.2.1 --target 127.0.0.1 --port 55999
```

---

## Repo layout

```
gre.sh                  # everything: setup iran/foreign, status, logs, update, panel
install.sh              # one-liner entry → gre.sh
panel/                  # Go single-binary web panel (main.go, setup*.go, index.html)
  grepanel              # server-side CLI
  README.md             # panel walkthrough
.github/workflows/     # build-panel.yml → prebuilt panel-rN releases
spoof_test.py           # spoof test helper
```
