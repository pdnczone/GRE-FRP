#!/bin/bash

# ==============================================================================
#   GRE + FRP Reverse Tunnel Automated Setup Script
#   Architecture: GRE Layer 3 Tunnel + FRP Reverse TLS Tunnel
#   Features: Auto Arch Detect, Systemd Auto-start on boot, MTU Clamping, TCP/UDP
# ==============================================================================

CYAN='\033[0;36m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/frp"
DEFAULT_FRP_VERSION="0.71.0"

# Default GRE internal IPs (/30 subnet)
IRAN_GRE_IP="10.10.10.2"
FOREIGN_GRE_IP="10.10.10.1"
TUNNEL_NAME="gre-tunnel"

check_root() {
    if [[ $EUID -ne 0 ]]; then
        echo -e "${RED}[!] This script must be run as root (sudo).${NC}"
        exit 1
    fi
}

detect_arch() {
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64)
            FRP_ARCH="amd64"
            ;;
        aarch64|arm64)
            FRP_ARCH="arm64"
            ;;
        armv7l|armhf)
            FRP_ARCH="arm"
            ;;
        *)
            echo -e "${RED}[!] Unsupported architecture: $ARCH${NC}"
            exit 1
            ;;
    esac
}

get_latest_frp_version() {
    LATEST_VER=$(curl -sSL --max-time 5 "https://api.github.com/repos/fatedier/frp/releases/latest" 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"v([^"]+)".*/\1/')
    if [[ -z "$LATEST_VER" ]]; then
        FRP_VERSION="$DEFAULT_FRP_VERSION"
    else
        FRP_VERSION="$LATEST_VER"
    fi
}

install_frp_binaries() {
    detect_arch
    get_latest_frp_version
    echo -e "${CYAN}[*] Downloading FRP v${FRP_VERSION} (${FRP_ARCH})...${NC}"

    mkdir -p "$CONFIG_DIR"
    TMP_DIR=$(mktemp -d)
    TAR_FILE="frp_${FRP_VERSION}_linux_${FRP_ARCH}.tar.gz"
    DOWNLOAD_URL="https://github.com/fatedier/frp/releases/download/v${FRP_VERSION}/${TAR_FILE}"

    if ! curl -sSL -o "${TMP_DIR}/${TAR_FILE}" "$DOWNLOAD_URL"; then
        echo -e "${RED}[!] Failed to download FRP from GitHub.${NC}"
        rm -rf "$TMP_DIR"
        exit 1
    fi

    tar -xzf "${TMP_DIR}/${TAR_FILE}" -C "$TMP_DIR"
    EXTRACTED_DIR="${TMP_DIR}/frp_${FRP_VERSION}_linux_${FRP_ARCH}"

    cp "${EXTRACTED_DIR}/frps" "$INSTALL_DIR/" 2>/dev/null
    cp "${EXTRACTED_DIR}/frpc" "$INSTALL_DIR/" 2>/dev/null
    chmod +x "${INSTALL_DIR}/frps" "${INSTALL_DIR}/frpc"

    rm -rf "$TMP_DIR"
    echo -e "${GREEN}[✔️] FRP installed to ${INSTALL_DIR}.${NC}"
}

setup_gre_systemd() {
    local LOCAL_IP=$1
    local REMOTE_IP=$2
    local GRE_INTERNAL_IP=$3

    echo -e "${CYAN}[*] Configuring persistent GRE tunnel service (${TUNNEL_NAME})...${NC}"

    # Tear down existing if present
    ip tunnel del "$TUNNEL_NAME" >/dev/null 2>&1 || true

    # Create systemd service for GRE
    cat <<EOF > /etc/systemd/system/${TUNNEL_NAME}.service
[Unit]
Description=GRE Tunnel Interface
After=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=-/sbin/ip tunnel del ${TUNNEL_NAME}
ExecStart=/bin/sh -c "/sbin/ip tunnel add ${TUNNEL_NAME} mode gre local ${LOCAL_IP} remote ${REMOTE_IP} ttl 255 && /sbin/ip link set dev ${TUNNEL_NAME} up mtu 1476 && /sbin/ip addr add ${GRE_INTERNAL_IP}/30 dev ${TUNNEL_NAME}"
ExecStop=-/sbin/ip tunnel del ${TUNNEL_NAME}

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "${TUNNEL_NAME}.service" >/dev/null 2>&1
    systemctl restart "${TUNNEL_NAME}.service"

    # Enable packet forwarding & MSS clamping to avoid fragmentation
    sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1
    iptables -t mangle -C POSTROUTING -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu >/dev/null 2>&1 || \
        iptables -t mangle -A POSTROUTING -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu

    echo -e "${GREEN}[✔️] GRE Tunnel service active with IP ${GRE_INTERNAL_IP}.${NC}"
}

setup_iran_server() {
    echo -e "\n${YELLOW}====================================================${NC}"
    echo -e "${YELLOW}       STEP 1: CONFIGURING IRAN SERVER (GRE + FRPS)  ${NC}"
    echo -e "${YELLOW}====================================================${NC}"

    # Prefer the local interface IP (what GRE must bind to) over the egress IP
    # an external service sees (often different behind NAT, e.g. ipify).
    MY_PUBLIC_IP=$(ip route get 1.1.1.1 2>/dev/null | awk '/src/ {for (i=1; i<=NF; i++) if ($i=="src") {print $(i+1); exit}}')
    [[ -z "$MY_PUBLIC_IP" ]] && MY_PUBLIC_IP=$(curl -sSL --max-time 5 https://api.ipify.org 2>/dev/null)
    read -p "Enter IRAN Server Public IP [Default: $MY_PUBLIC_IP]: " IP_IRAN
    IP_IRAN=${IP_IRAN:-$MY_PUBLIC_IP}

    read -p "Enter FOREIGN Server Public IP: " IP_FOREIGN
    while [[ -z "$IP_FOREIGN" ]]; do
        read -p "FOREIGN Server IP cannot be empty. Enter IP: " IP_FOREIGN
    done

    # 1. Setup GRE Tunnel
    setup_gre_systemd "$IP_IRAN" "$IP_FOREIGN" "$IRAN_GRE_IP"

    # 2. Setup FRP Server (frps)
    install_frp_binaries

    read -p "Enter FRP Bind Port [Default: 7000]: " BIND_PORT
    BIND_PORT=${BIND_PORT:-7000}

    AUTO_TOKEN=$(tr -dc A-Za-z0-9 </dev/urandom | head -c 16 2>/dev/null || openssl rand -hex 8)
    read -p "Enter Secret Auth Token [Press Enter for: $AUTO_TOKEN]: " TOKEN
    TOKEN=${TOKEN:-$AUTO_TOKEN}

    cat <<EOF > "${CONFIG_DIR}/frps.toml"
bindAddr = "0.0.0.0"
bindPort = ${BIND_PORT}
auth.method = "token"
auth.token = "${TOKEN}"
transport.tls.force = false
EOF

    cat <<EOF > /etc/systemd/system/frps.service
[Unit]
Description=FRP Server Service
After=network.target ${TUNNEL_NAME}.service
Wants=${TUNNEL_NAME}.service

[Service]
Type=simple
User=root
Restart=always
RestartSec=5s
ExecStart=${INSTALL_DIR}/frps -c ${CONFIG_DIR}/frps.toml

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable frps >/dev/null 2>&1
    systemctl restart frps

    # Allow firewall if ufw is active
    if command -v ufw >/dev/null 2>&1 && ufw status | grep -q "Status: active"; then
        ufw allow "${BIND_PORT}/tcp" >/dev/null 2>&1
    fi

    echo -e "\n${GREEN}=================================================================${NC}"
    echo -e "${GREEN}[✔️] IRAN SERVER CONFIGURATION COMPLETE!${NC}"
    echo -e "GRE Public Link:      ${CYAN}${IP_IRAN} <--> ${IP_FOREIGN}${NC}"
    echo -e "IRAN GRE Internal IP: ${CYAN}${IRAN_GRE_IP}${NC}"
    echo -e "FRP Bind Port:        ${CYAN}${BIND_PORT}${NC}"
    echo -e "Secret Token:         ${CYAN}${TOKEN}${NC}"
    echo -e "\n${YELLOW}>>> Now run this script on FOREIGN server and provide:${NC}"
    echo -e "1. IRAN Public IP: ${CYAN}${IP_IRAN}${NC}"
    echo -e "2. Port:           ${CYAN}${BIND_PORT}${NC}"
    echo -e "3. Token:          ${CYAN}${TOKEN}${NC}"
    echo -e "${GREEN}=================================================================${NC}\n"
}

setup_foreign_server() {
    echo -e "\n${YELLOW}====================================================${NC}"
    echo -e "${YELLOW}   STEP 2: CONFIGURING FOREIGN SERVER (GRE + FRPC)  ${NC}"
    echo -e "${YELLOW}====================================================${NC}"
    MY_PUBLIC_IP=$(ip route get 1.1.1.1 2>/dev/null | awk '/src/ {for (i=1; i<=NF; i++) if ($i=="src") {print $(i+1); exit}}')
    [[ -z "$MY_PUBLIC_IP" ]] && MY_PUBLIC_IP=$(curl -sSL --max-time 5 https://api.ipify.org 2>/dev/null)
    read -p "Enter FOREIGN Server Public IP [Default: $MY_PUBLIC_IP]: " IP_FOREIGN
    IP_FOREIGN=${IP_FOREIGN:-$MY_PUBLIC_IP}

    read -p "Enter IRAN Server Public IP: " IP_IRAN
    while [[ -z "$IP_IRAN" ]]; do
        read -p "IRAN Server IP cannot be empty. Enter IP: " IP_IRAN
    done

    # 1. Setup GRE Tunnel
    setup_gre_systemd "$IP_FOREIGN" "$IP_IRAN" "$FOREIGN_GRE_IP"

    # Test GRE Connectivity via Ping
    echo -e "${CYAN}[*] Testing GRE internal ping to Iran (${IRAN_GRE_IP})...${NC}"
    if ping -c 3 -W 2 "$IRAN_GRE_IP" >/dev/null 2>&1; then
        echo -e "${GREEN}[✔️] GRE Tunnel link is UP and reachable!${NC}"
    else
        echo -e "${YELLOW}[!] Warning: Ping to ${IRAN_GRE_IP} did not respond yet.${NC}"
        echo -e "${YELLOW}    (Make sure you configured IRAN server first and ICMP is allowed).${NC}"
    fi

    # 2. Setup FRP Client (frpc)
    install_frp_binaries

    read -p "Enter FRP Bind Port [Default: 7000]: " SERVER_PORT
    SERVER_PORT=${SERVER_PORT:-7000}

    read -p "Enter Secret Auth Token: " TOKEN
    while [[ -z "$TOKEN" ]]; do
        read -p "Token cannot be empty. Enter Token: " TOKEN
    done

    read -p "Enter Ports to Reverse-Tunnel (e.g. 443, 2083, 8080): " INPUT_PORTS
    while [[ -z "$INPUT_PORTS" ]]; do
        read -p "Please enter at least one port: " INPUT_PORTS
    done

    # We connect frpc to Iran's GRE internal IP ($IRAN_GRE_IP) through the GRE tunnel!
    cat <<EOF > "${CONFIG_DIR}/frpc.toml"
serverAddr = "${IRAN_GRE_IP}"
serverPort = ${SERVER_PORT}
auth.method = "token"
auth.token = "${TOKEN}"
transport.tls.enable = true

EOF

    PORTS_CLEANED=$(echo "$INPUT_PORTS" | tr ',' ' ')
    for PORT in $PORTS_CLEANED; do
        if [[ "$PORT" =~ ^[0-9]+$ ]]; then
            cat <<EOF >> "${CONFIG_DIR}/frpc.toml"
[[proxies]]
name = "tcp_${PORT}"
type = "tcp"
localIP = "127.0.0.1"
localPort = ${PORT}
remotePort = ${PORT}

[[proxies]]
name = "udp_${PORT}"
type = "udp"
localIP = "127.0.0.1"
localPort = ${PORT}
remotePort = ${PORT}

EOF
        fi
    done

    cat <<EOF > /etc/systemd/system/frpc.service
[Unit]
Description=FRP Client Reverse Service
After=network.target ${TUNNEL_NAME}.service
Wants=${TUNNEL_NAME}.service

[Service]
Type=simple
User=root
Restart=always
RestartSec=5s
ExecStart=${INSTALL_DIR}/frpc -c ${CONFIG_DIR}/frpc.toml

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable frpc >/dev/null 2>&1
    systemctl restart frpc

    echo -e "\n${GREEN}=================================================================${NC}"
    echo -e "${GREEN}[✔️] FOREIGN SERVER CONFIGURATION COMPLETE!${NC}"
    echo -e "GRE Public Link:      ${CYAN}${IP_FOREIGN} <--> ${IP_IRAN}${NC}"
    echo -e "FOREIGN GRE IP:       ${CYAN}${FOREIGN_GRE_IP}${NC}"
    echo -e "FRP Connecting to:    ${CYAN}${IRAN_GRE_IP}:${SERVER_PORT}${NC} (Inside GRE Tunnel)"
    echo -e "Reverse Ports:        ${CYAN}${PORTS_CLEANED}${NC} (TCP & UDP)"
    echo -e "FRP TLS Encryption:   ${GREEN}Enabled${NC}"
    echo -e "${GREEN}=================================================================${NC}\n"
}

check_status() {
    echo -e "\n${YELLOW}=== Checking GRE & FRP Status ===${NC}"

    # 1. GRE Status
    echo -e "\n${CYAN}[1] GRE Tunnel Interface:${NC}"
    if ip link show "$TUNNEL_NAME" >/dev/null 2>&1; then
        ip addr show dev "$TUNNEL_NAME"
        echo -e "${GREEN}[✔️] Interface ${TUNNEL_NAME} exists and is UP.${NC}"
    else
        echo -e "${RED}[!] Interface ${TUNNEL_NAME} NOT found.${NC}"
    fi

    # 2. Ping Test
    echo -e "\n${CYAN}[2] GRE Ping Test:${NC}"
    if ip addr show dev "$TUNNEL_NAME" 2>/dev/null | grep -q "$IRAN_GRE_IP"; then
        TARGET_PING="$FOREIGN_GRE_IP"
        echo "Testing ping to Foreign GRE IP ($TARGET_PING)..."
    else
        TARGET_PING="$IRAN_GRE_IP"
        echo "Testing ping to Iran GRE IP ($TARGET_PING)..."
    fi
    ping -c 3 -W 2 "$TARGET_PING" && echo -e "${GREEN}[✔️] Ping OK.${NC}" || echo -e "${YELLOW}[!] Remote peer did not answer ping.${NC}"

    # 3. FRP Service Status
    echo -e "\n${CYAN}[3] FRP Service Status:${NC}"
    if systemctl is-active --quiet frps; then
        echo -e "${GREEN}[✔️] frps (Server on IRAN) is ACTIVE and RUNNING.${NC}"
        systemctl status frps --no-pager -l
    elif systemctl is-active --quiet frpc; then
        echo -e "${GREEN}[✔️] frpc (Client on FOREIGN) is ACTIVE and RUNNING.${NC}"
        systemctl status frpc --no-pager -l
    else
        echo -e "${RED}[!] Neither frps nor frpc is active.${NC}"
    fi
}

show_logs() {
    echo -e "\n${YELLOW}=== Live Service Logs (Ctrl+C to exit) ===${NC}"
    if systemctl list-unit-files | grep -q "frps.service"; then
        journalctl -u frps -n 50 -f
    elif systemctl list-unit-files | grep -q "frpc.service"; then
        journalctl -u frpc -n 50 -f
    else
        echo -e "${RED}[!] No FRP service found.${NC}"
    fi
}

restart_all() {
    echo -e "\n${CYAN}[*] Restarting GRE and FRP services...${NC}"
    systemctl restart "${TUNNEL_NAME}.service" >/dev/null 2>&1
    systemctl restart frps >/dev/null 2>&1
    systemctl restart frpc >/dev/null 2>&1
    echo -e "${GREEN}[✔️] All services restarted.${NC}"
}

uninstall_all() {
    echo -e "\n${RED}=== Uninstalling GRE + FRP Tunnel ===${NC}"
    read -p "Are you sure you want to completely remove GRE & FRP? (y/N): " CONFIRM
    if [[ "$CONFIRM" =~ ^[Yy]$ ]]; then
        # Stop & disable services
        systemctl stop frps frpc "${TUNNEL_NAME}.service" >/dev/null 2>&1
        systemctl disable frps frpc "${TUNNEL_NAME}.service" >/dev/null 2>&1

        # Remove systemd files
        rm -f /etc/systemd/system/frps.service /etc/systemd/system/frpc.service /etc/systemd/system/${TUNNEL_NAME}.service
        systemctl daemon-reload

        # Remove GRE interface
        ip tunnel del "$TUNNEL_NAME" >/dev/null 2>&1 || true

        # Remove binaries & configs
        rm -f "${INSTALL_DIR}/frps" "${INSTALL_DIR}/frpc"
        rm -rf "$CONFIG_DIR"

        echo -e "${GREEN}[✔️] GRE & FRP completely uninstalled.${NC}"
    else
        echo -e "${YELLOW}[*] Aborted.${NC}"
    fi
}

main_menu() {
    clear
    echo -e "${CYAN}"
    echo "=========================================================="
    echo "       GRE + FRP Reverse Tunnel Manager (Iran <-> Kharej)"
    echo "     Layer 3 GRE Tunnel + Encrypted TLS FRP Reverse Relay"
    echo "=========================================================="
    echo -e "${NC}"
    echo "1) Setup IRAN Server    (GRE + FRP Server / frps)"
    echo "2) Setup FOREIGN Server (GRE + FRP Client / frpc Reverse)"
    echo "3) Check Connection Status & GRE Ping Test"
    echo "4) View FRP Live Logs"
    echo "5) Restart Tunnel Services"
    echo "6) Uninstall Everything (GRE + FRP)"
    echo "0) Exit"
    echo ""
    read -p "Select an option [0-6]: " OPTION

    case "$OPTION" in
        1)
            setup_iran_server
            ;;
        2)
            setup_foreign_server
            ;;
        3)
            check_status
            ;;
        4)
            show_logs
            ;;
        5)
            restart_all
            ;;
        6)
            uninstall_all
            ;;
        0)
            echo "Exiting..."
            exit 0
            ;;
        *)
            echo -e "${RED}[!] Invalid option.${NC}"
            ;;
    esac
}

check_root
main_menu
