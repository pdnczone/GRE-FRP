# GRE-FRP Panel

Web control panel for the GRE+FRP tunnel stack (inspired by Hashem panel).
Single Go binary, no dependencies. Runs on the Iran server; controls the
Turkey peer remotely through its own panel over the GRE private network.

## Build

```bash
cd panel
go build -o gre-panel .
```

## Run

```bash
sudo ./gre-panel            # listens on :7777 under a secret path
sudo GRE_PANEL_DIR=/etc/gre-panel ./gre-panel
```

First start prints an 8-digit password (`admin` user). Change it from Settings.
For tests/dev: `GRE_PANEL_PASSWORD=... ./gre-panel` sets a fixed password.

## Install as a service

```bash
sudo cp gre-panel /usr/local/bin/
sudo tee /etc/systemd/system/gre-panel.service <<'UNIT'
[Unit]
Description=GRE-FRP Panel
After=network.target

[Service]
Type=simple
User=root
Restart=always
RestartSec=5s
ExecStart=/usr/local/bin/gre-panel

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable --now gre-panel
sudo cat /etc/gre-panel/panel.json   # get the secret path + open the URL
```

## Peer setup (Iran panel → Turkey)

1. Install the same binary on the Turkey server and start it.
2. In the Iran panel → Peer: enter the Turkey panel URL **with its secret path**
   (use the GRE inner IP, e.g. `http://10.10.10.1:7777/<secret>`), user + password.
3. The Tunnel card then shows both sides.
