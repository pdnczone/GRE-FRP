# GRE + FRP Reverse Tunnel

GRE Layer 3 tunnel + FRP reverse TLS tunnel (Iran ↔ Foreign).

## Usage

```bash
bash <(curl -sSL https://raw.githubusercontent.com/pdnczone/GRE-FRP/main/gre.sh)
```

Or clone and run:

```bash
git clone https://github.com/pdnczone/GRE-FRP.git
cd GRE-FRP
sudo bash gre.sh
```

## Menu

| Option | Action                                     |
|--------|--------------------------------------------|
| 1      | Setup IRAN server (GRE + frps)             |
| 2      | Setup FOREIGN server (GRE + frpc reverse)  |
| 3      | Check connection status & GRE ping test    |
| 4      | View FRP live logs                         |
| 5      | Restart tunnel services                    |
| 6      | Uninstall everything (GRE + FRP)           |

## Architecture

- GRE tunnel: Iran `10.10.10.2/30` ↔ Foreign `10.10.10.1/30` (systemd persistent, MTU 1476, MSS clamping)
- FRP: frps binds `0.0.0.0`, frpc connects via GRE internal IP, TLS enabled, TCP+UDP per port
