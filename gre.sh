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

# ---- input validation (same rules as the web panel: IPv4, port 1-65535) ----
is_valid_ip() {
    [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    local IFS=. a b c d o
    read -r a b c d <<<"$1"
    for o in "$a" "$b" "$c" "$d"; do
        ((10#$o <= 255)) || return 1
    done
}

is_valid_port() {
    [[ "$1" =~ ^[0-9]+$ ]] && ((10#$1 >= 1 && 10#$1 <= 65535))
}

prompt_ip() { # $1=varname $2=label $3=default (empty = required)
    local __var=$1 __label=$2 __def=$3 __in
    while true; do
        if [[ -n "$__def" ]]; then
            read -p "$__label [Default: $__def]: " __in
            __in=${__in:-$__def}
        else
            read -p "$__label: " __in
        fi
        if is_valid_ip "$__in"; then printf -v "$__var" '%s' "$__in"; return 0; fi
        echo -e "${RED}[!] Invalid IPv4 address: '${__in}'. Example: 203.0.113.10${NC}"
    done
}

prompt_port() { # $1=varname $2=label $3=default
    local __var=$1 __label=$2 __def=$3 __in
    while true; do
        read -p "$__label [Default: $__def]: " __in
        __in=${__in:-$__def}
        if is_valid_port "$__in"; then printf -v "$__var" '%s' "$((10#$__in))"; return 0; fi
        echo -e "${RED}[!] Invalid port: '${__in}'. Must be 1-65535.${NC}"
    done
}

prompt_required() { # $1=varname $2=label — must be non-empty
    local __var=$1 __label=$2 __in
    while true; do
        read -p "$__label: " __in
        if [[ -n "$__in" ]]; then printf -v "$__var" '%s' "$__in"; return 0; fi
        echo -e "${RED}[!] This field is required and cannot be empty.${NC}"
    done
}

prompt_token() { # $1=varname $2=label $3=default (empty accepts default)
    local __var=$1 __label=$2 __def=$3 __in
    read -p "$__label [Press Enter for: $__def]: " __in
    printf -v "$__var" '%s' "${__in:-$__def}"
}

prompt_ports() { # $1=varname $2=label — at least one valid port
    local __var=$1 __label=$2 __in __ok p
    while true; do
        read -p "$__label (e.g. 443, 2083, 8080): " __in
        __ok=""
        for p in $(echo "$__in" | tr ',' ' '); do
            is_valid_port "$p" && __ok="$__ok $((10#$p))"
        done
        __ok=$(echo "$__ok" | xargs)
        if [[ -n "$__ok" ]]; then printf -v "$__var" '%s' "$__ok"; return 0; fi
        echo -e "${RED}[!] Enter at least one valid port (1-65535).${NC}"
    done
}

# validate_setup_common checks non-interactive args with the same rules as
# the prompts above. Prints a clear error per bad field, returns non-zero.
validate_setup_common() { # $1=local_pub $2=remote_pub $3=frp_port $4=local_gre
    local ok=1
    is_valid_ip "$1" || { echo -e "${RED}[!] Invalid local public IP: '$1'${NC}"; ok=0; }
    is_valid_ip "$2" || { echo -e "${RED}[!] Invalid remote public IP: '$2'${NC}"; ok=0; }
    is_valid_port "$3" || { echo -e "${RED}[!] Invalid FRP port: '$3' (must be 1-65535)${NC}"; ok=0; }
    is_valid_ip "$4" || { echo -e "${RED}[!] Invalid local GRE IP: '$4'${NC}"; ok=0; }
    return $((1 - ok))
}

tunnel_present() {
    ip tunnel show 2>/dev/null | grep -q "$TUNNEL_NAME" && return 0
    [[ -f "${CONFIG_DIR}/frps.toml" || -f "${CONFIG_DIR}/frpc.toml" ]] && return 0
    return 1
}

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
ExecStart=/bin/sh -c "/sbin/ip tunnel add ${TUNNEL_NAME} mode gre local ${LOCAL_IP} remote ${REMOTE_IP} ttl 255 && /sbin/ip link set dev ${TUNNEL_NAME} up mtu 1448 && /sbin/ip addr add ${GRE_INTERNAL_IP}/30 dev ${TUNNEL_NAME}"
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

# ---- SINGLE SOURCE OF TRUTH for install logic ----
# setup_iran_server_noninteractive / setup_foreign_server_noninteractive do the
# real work. The interactive menu functions below only prompt + validate, then
# delegate here. The web panel calls the same functions via the CLI flags at
# the bottom of this file (setup-iran / setup-foreign), so all three paths
# (menu, CLI, panel) execute identical steps.
# Args: $1=local_pub $2=remote_pub $3=frp_port $4=token [$5=local_gre [$6=peer_gre [$7="cleaned ports"]]]
setup_iran_server_noninteractive() {
    local IP_IRAN=$1 IP_FOREIGN=$2 BIND_PORT=$3 TOKEN=$4
    local LOCAL_GRE=${5:-$IRAN_GRE_IP} PEER_GRE=${6:-$FOREIGN_GRE_IP}
    setup_gre_systemd "$IP_IRAN" "$IP_FOREIGN" "$LOCAL_GRE"
    install_frp_binaries
    cat <<EOF > "${CONFIG_DIR}/frps.toml"
bindAddr = "0.0.0.0"
bindPort = ${BIND_PORT}
auth.method = "token"
auth.token = "${TOKEN}"
transport.tls.force = false
transport.maxPoolCount = 50
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
    if command -v ufw >/dev/null 2>&1 && ufw status | grep -q "Status: active"; then
        ufw allow "${BIND_PORT}/tcp" >/dev/null 2>&1
    fi
    echo -e "${GREEN}[✔️] IRAN setup done: GRE ${IP_IRAN} <-> ${IP_FOREIGN} (${LOCAL_GRE} peer ${PEER_GRE}), frps :${BIND_PORT}${NC}"
    echo -e "${YELLOW}Token: ${TOKEN} (copy to the FOREIGN side)${NC}"
    if [[ "${GRE_SKIP_PANEL:-0}" == "1" ]]; then
        echo -e "${CYAN}[*] Skipping panel install (called from panel).${NC}"
    else
        install_panel || echo -e "${YELLOW}[!] Panel auto-install failed — retry from menu option 7.${NC}"
    fi
}

setup_foreign_server_noninteractive() {
    local IP_FOREIGN=$1 IP_IRAN=$2 SERVER_PORT=$3 TOKEN=$4
    local LOCAL_GRE=${5:-$FOREIGN_GRE_IP} PEER_GRE=${6:-$IRAN_GRE_IP}
    local PORTS_CLEANED=${7:-}
    _setup_foreign_full "$IP_FOREIGN" "$IP_IRAN" "$SERVER_PORT" "$TOKEN" "$LOCAL_GRE" "$PEER_GRE" "$PORTS_CLEANED"
}

# shared full foreign path: GRE + ping feedback + frpc binaries/config/service + panel.
# Called by the interactive menu, the CLI, and (via CLI) the web panel.
_setup_foreign_full() {
    local IP_FOREIGN=$1 IP_IRAN=$2 SERVER_PORT=$3 TOKEN=$4
    local LOCAL_GRE=$5 PEER_GRE=$6 PORTS_CLEANED=$7
    setup_gre_systemd "$IP_FOREIGN" "$IP_IRAN" "$LOCAL_GRE"
    echo -e "${CYAN}[*] Testing GRE internal ping to Iran (${PEER_GRE})...${NC}"
    if ping -c 3 -W 2 "$PEER_GRE" >/dev/null 2>&1; then
        echo -e "${GREEN}[✔️] GRE Tunnel link is UP and reachable!${NC}"
    else
        echo -e "${YELLOW}[!] Warning: Ping to ${PEER_GRE} did not respond yet.${NC}"
    fi
    install_frp_binaries
    cat <<EOF > "${CONFIG_DIR}/frpc.toml"
serverAddr = "${PEER_GRE}"
serverPort = ${SERVER_PORT}
auth.method = "token"
auth.token = "${TOKEN}"
transport.tls.enable = true
transport.poolCount = 10

EOF
    local PORT
    for PORT in $PORTS_CLEANED; do
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
    echo -e "${GREEN}[✔️] FOREIGN setup done: GRE ${IP_FOREIGN} <-> ${IP_IRAN} (${LOCAL_GRE} peer ${PEER_GRE}), frpc → ${PEER_GRE}:${SERVER_PORT}${NC}"
    echo -e "${GREEN}Reverse ports: ${PORTS_CLEANED} (TCP & UDP, TLS)${NC}"
    if [[ "${GRE_SKIP_PANEL:-0}" == "1" ]]; then
        echo -e "${CYAN}[*] Skipping panel install (called from panel).${NC}"
    else
        install_panel || echo -e "${YELLOW}[!] Panel auto-install failed — retry from menu option 7.${NC}"
    fi
}

setup_iran_server() {
    echo -e "\n${YELLOW}====================================================${NC}"
    echo -e "${YELLOW}       STEP 1: CONFIGURING IRAN SERVER (GRE + FRPS)  ${NC}"
    echo -e "${YELLOW}====================================================${NC}"

    # Prefer the local interface IP (what GRE must bind to) over the egress IP
    # an external service sees (often different behind NAT, e.g. ipify).
    MY_PUBLIC_IP=$(ip route get 1.1.1.1 2>/dev/null | awk '/src/ {for (i=1; i<=NF; i++) if ($i=="src") {print $(i+1); exit}}')
    [[ -z "$MY_PUBLIC_IP" ]] && MY_PUBLIC_IP=$(curl -sSL --max-time 5 https://api.ipify.org 2>/dev/null)
    prompt_ip IP_IRAN "Enter IRAN Server Public IP" "$MY_PUBLIC_IP"
    prompt_ip IP_FOREIGN "Enter FOREIGN Server Public IP" ""

    prompt_port BIND_PORT "Enter FRP Bind Port" "7000"

    AUTO_TOKEN=$(tr -dc A-Za-z0-9 </dev/urandom | head -c 16 2>/dev/null || openssl rand -hex 8)
    prompt_token TOKEN "Enter Secret Auth Token" "$AUTO_TOKEN"

    # single source of truth: GRE + frps + panel all happen inside
    setup_iran_server_noninteractive "$IP_IRAN" "$IP_FOREIGN" "$BIND_PORT" "$TOKEN" "$IRAN_GRE_IP" "$FOREIGN_GRE_IP"

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

    # panel is already running here (menu path) — install it fresh
    install_panel || echo -e "${YELLOW}[!] Panel auto-install failed — retry from menu option 7.${NC}"
}

setup_foreign_server() {
    echo -e "\n${YELLOW}====================================================${NC}"
    echo -e "${YELLOW}   STEP 2: CONFIGURING FOREIGN SERVER (GRE + FRPC)  ${NC}"
    echo -e "${YELLOW}====================================================${NC}"
    MY_PUBLIC_IP=$(ip route get 1.1.1.1 2>/dev/null | awk '/src/ {for (i=1; i<=NF; i++) if ($i=="src") {print $(i+1); exit}}')
    [[ -z "$MY_PUBLIC_IP" ]] && MY_PUBLIC_IP=$(curl -sSL --max-time 5 https://api.ipify.org 2>/dev/null)
    prompt_ip IP_FOREIGN "Enter FOREIGN Server Public IP" "$MY_PUBLIC_IP"
    prompt_ip IP_IRAN "Enter IRAN Server Public IP" ""

    prompt_port SERVER_PORT "Enter FRP Bind Port" "7000"
    prompt_required TOKEN "Enter Secret Auth Token"
    prompt_ports INPUT_PORTS "Enter Ports to Reverse-Tunnel"

    # single source of truth: GRE + ping + frpc + panel all happen inside
    # (frpc reaches Iran's GRE internal IP through the GRE tunnel)
    PORTS_CLEANED=$(echo "$INPUT_PORTS" | tr ',' ' ')
    _setup_foreign_full "$IP_FOREIGN" "$IP_IRAN" "$SERVER_PORT" "$TOKEN" "$FOREIGN_GRE_IP" "$IRAN_GRE_IP" "$PORTS_CLEANED"

    echo -e "\n${GREEN}=================================================================${NC}"
    echo -e "${GREEN}[✔️] FOREIGN SERVER CONFIGURATION COMPLETE!${NC}"
    echo -e "GRE Public Link:      ${CYAN}${IP_FOREIGN} <--> ${IP_IRAN}${NC}"
    echo -e "FOREIGN GRE IP:       ${CYAN}${FOREIGN_GRE_IP}${NC}"
    echo -e "FRP Connecting to:    ${CYAN}${IRAN_GRE_IP}:${SERVER_PORT}${NC} (Inside GRE Tunnel)"
    echo -e "Reverse Ports:        ${CYAN}${PORTS_CLEANED}${NC} (TCP & UDP)"
    echo -e "FRP TLS Encryption:   ${GREEN}Enabled${NC}"
    echo -e "${GREEN}=================================================================${NC}\n"

    # panel is already running here (menu path) — install it fresh
    install_panel || echo -e "${YELLOW}[!] Panel auto-install failed — retry from menu option 7.${NC}"
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

remove_tunnel() {
    echo -e "\n${RED}=== Removing GRE + FRP Tunnel (panel stays) ===${NC}"
    read -p "Remove the tunnel from THIS server? Panel stays installed. (y/N): " CONFIRM
    if [[ "$CONFIRM" =~ ^[Yy]$ ]]; then
        remove_tunnel_force
    else
        echo -e "${YELLOW}[*] Aborted.${NC}"
    fi
}

# Non-interactive core: stop/disable units, drop interface, remove FRP files.
# Panel files/services are never touched here.
remove_tunnel_force() {
        # Stop & disable services
        systemctl stop frps frpc "${TUNNEL_NAME}.service" >/dev/null 2>&1
        systemctl disable frps frpc "${TUNNEL_NAME}.service" >/dev/null 2>&1

        # Remove systemd files
        rm -f /etc/systemd/system/frps.service /etc/systemd/system/frpc.service /etc/systemd/system/${TUNNEL_NAME}.service
        systemctl daemon-reload
        systemctl reset-failed >/dev/null 2>&1 || true

        # Remove GRE interface
        ip tunnel del "$TUNNEL_NAME" >/dev/null 2>&1 || true

        # Remove binaries & configs (panel untouched)
        rm -f "${INSTALL_DIR}/frps" "${INSTALL_DIR}/frpc"
        rm -rf "$CONFIG_DIR"

        echo -e "${GREEN}[✔️] Tunnel removed — GRE interface, FRP services, binaries and configs gone. Panel still running.${NC}"
}

PANEL_DIR="/usr/local/gre-panel"
PANEL_BIN="/usr/local/bin/gre-panel"

# ---- Network optimization for tunnel throughput ----
# Same on both roles (auto-detects nothing: these are role-independent).
# Backup lives in /etc/gre-panel/tune.bak (key=value snapshot), restored by
# tune_restore(). Idempotent — safe to run twice.
TUNE_BACKUP="/etc/gre-panel/tune.bak"

tune_backup_once() {
    if [[ -f "$TUNE_BACKUP" ]]; then return 0; fi
    mkdir -p "$(dirname "$TUNE_BACKUP")"
    : > "$TUNE_BACKUP"
    local k v
    for k in net.ipv4.ip_forward net.core.rmem_max net.core.wmem_max \
             net.core.netdev_max_backlog net.ipv4.tcp_congestion_control; do
        v=$(sysctl -n "$k" 2>/dev/null) || v=""
        echo "$k=$v" >> "$TUNE_BACKUP"
    done
    if lsmod 2>/dev/null | grep -q "^tcp_bbr"; then echo "tcp_bbr=loaded" >> "$TUNE_BACKUP";
    else echo "tcp_bbr=absent" >> "$TUNE_BACKUP"; fi
    echo "gre_mtu=$(ip link show "$TUNNEL_NAME" 2>/dev/null | grep -o 'mtu [0-9]*' | awk '{print $2}')" >> "$TUNE_BACKUP"
    if iptables -t mangle -C POSTROUTING -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu >/dev/null 2>&1; then
        echo "mss_clamp=present" >> "$TUNE_BACKUP"
    else
        echo "mss_clamp=absent" >> "$TUNE_BACKUP"
    fi
    echo -e "${CYAN}[*] Current settings backed up to ${TUNE_BACKUP}.${NC}"
}

tune_apply() {
    tune_backup_once
    echo -e "${CYAN}[*] Optimizing network stack for tunnel throughput...${NC}"

    # 1. BBR congestion control (best for high-latency links like IR↔TR)
    if modprobe tcp_bbr >/dev/null 2>&1 || lsmod 2>/dev/null | grep -q "^tcp_bbr"; then
        sysctl -w net.ipv4.tcp_congestion_control=bbr >/dev/null 2>&1 && echo -e "${GREEN}[✔️] TCP congestion control → bbr${NC}" || echo -e "${YELLOW}[!] bbr unavailable — keeping current CC.${NC}"
    else
        echo -e "${YELLOW}[!] tcp_bbr module not available — keeping current CC.${NC}"
    fi

    # 2. Bigger socket buffers (16MB) so fast links don't stall
    sysctl -w net.core.rmem_max=16777216 >/dev/null 2>&1
    sysctl -w net.core.wmem_max=16777216 >/dev/null 2>&1
    echo -e "${GREEN}[✔️] Socket buffers → 16MB (rmem_max/wmem_max)${NC}"

    # 3. Deeper NIC queue (packet bursts under load)
    sysctl -w net.core.netdev_max_backlog=5000 >/dev/null 2>&1
    echo -e "${GREEN}[✔️] netdev backlog → 5000${NC}"

    # 4. IP forwarding (tunnel needs it)
    sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1
    echo -e "${GREEN}[✔️] IPv4 forwarding → on${NC}"

    # 5. GRE MTU 1448 (1500 outer − 24 GRE − 28 IP/ICMP headroom:
    # full-size packets pass unfragmented, verified by MTU probe)
    if ip link show "$TUNNEL_NAME" >/dev/null 2>&1; then
        ip link set dev "$TUNNEL_NAME" mtu 1448 >/dev/null 2>&1 && echo -e "${GREEN}[✔️] ${TUNNEL_NAME} MTU → 1448${NC}" || echo -e "${YELLOW}[!] Could not set GRE MTU.${NC}"
    else
        echo -e "${YELLOW}[*] No ${TUNNEL_NAME} interface yet — MTU will apply on next setup.${NC}"
    fi

    # 6. MSS clamp (idempotent) so TCP never fragments through the tunnel
    iptables -t mangle -C POSTROUTING -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu >/dev/null 2>&1 || \
        iptables -t mangle -A POSTROUTING -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
    echo -e "${GREEN}[✔️] TCP MSS clamp → on${NC}"

    # 7. Persist across reboots
    mkdir -p /etc/sysctl.d
    cat > /etc/sysctl.d/99-gre-tune.conf <<'EOF'
# GRE-FRP tunnel optimization (applied by Optimize button / tune command)
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.core.netdev_max_backlog = 5000
net.ipv4.ip_forward = 1
EOF
    if sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null | grep -q bbr; then
        echo "net.ipv4.tcp_congestion_control = bbr" >> /etc/sysctl.d/99-gre-tune.conf
    fi
    echo -e "${GREEN}[✔️] Settings persisted in /etc/sysctl.d/99-gre-tune.conf${NC}"
    echo -e "${GREEN}[✔️] Optimization done — run Restore if anything feels worse.${NC}"
}

tune_restore() {
    if [[ ! -f "$TUNE_BACKUP" ]]; then
        echo -e "${YELLOW}[!] No backup found at ${TUNE_BACKUP} — nothing to restore.${NC}"
        return 1
    fi
    echo -e "${CYAN}[*] Restoring pre-optimization settings...${NC}"
    local k v
    while IFS='=' read -r k v; do
        case "$k" in
            net.*) [[ -n "$v" ]] && sysctl -w "$k=$v" >/dev/null 2>&1 && echo -e "${GREEN}[✔️] $k → $v${NC}" ;;
            gre_mtu)
                if [[ -n "$v" ]] && ip link show "$TUNNEL_NAME" >/dev/null 2>&1; then
                    ip link set dev "$TUNNEL_NAME" mtu "$v" >/dev/null 2>&1 && echo -e "${GREEN}[✔️] ${TUNNEL_NAME} MTU → $v${NC}"
                fi ;;
            mss_clamp)
                if [[ "$v" == "absent" ]]; then
                    iptables -t mangle -D POSTROUTING -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu >/dev/null 2>&1 || true
                    echo -e "${GREEN}[✔️] MSS clamp removed${NC}"
                fi ;;
        esac
    done < "$TUNE_BACKUP"
    rm -f /etc/sysctl.d/99-gre-tune.conf
    echo -e "${GREEN}[✔️] Restored — backup kept at ${TUNE_BACKUP} (deleted on next optimize run).${NC}"
    rm -f "$TUNE_BACKUP"
}

tune_status() {
    echo -e "${CYAN}=== Tunnel Optimization Status ===${NC}"
    echo "CC:        $(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null || echo ?)"
    echo "rmem_max:  $(sysctl -n net.core.rmem_max 2>/dev/null || echo ?)"
    echo "wmem_max:  $(sysctl -n net.core.wmem_max 2>/dev/null || echo ?)"
    echo "backlog:   $(sysctl -n net.core.netdev_max_backlog 2>/dev/null || echo ?)"
    echo "forward:   $(sysctl -n net.ipv4.ip_forward 2>/dev/null || echo ?)"
    echo "GRE MTU:   $(ip link show "$TUNNEL_NAME" 2>/dev/null | grep -o 'mtu [0-9]*' | awk '{print $2}' || echo 'no interface')"
    if iptables -t mangle -C POSTROUTING -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu >/dev/null 2>&1; then
        echo "MSS clamp: on"
    else
        echo "MSS clamp: off"
    fi
    if [[ -f "$TUNE_BACKUP" ]]; then echo "Backup:    $TUNE_BACKUP (restore available)"; else echo "Backup:    none"; fi
    [[ -f /etc/sysctl.d/99-gre-tune.conf ]] && echo "Persisted: yes (/etc/sysctl.d/99-gre-tune.conf)" || echo "Persisted: no"
}

install_panel() {
    echo -e "${CYAN}[*] Installing GRE-FRP web panel...${NC}"

    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64)  PANEL_ASSET="gre-panel-linux-amd64" ;;
        aarch64|arm64) PANEL_ASSET="gre-panel-linux-arm64" ;;
        *) echo -e "${RED}[!] Unsupported arch for panel: $ARCH${NC}"; return 1 ;;
    esac

    TMP_PANEL="$(mktemp -d)"
    DL_OK=0
    # try latest release first (prebuilt, no Go needed)
    LATEST_JSON=$(curl -fsSL --max-time 15 "https://api.github.com/repos/pdnczone/GRE-FRP/releases/latest" 2>/dev/null) || true
    if [[ -n "$LATEST_JSON" ]]; then
        DL_URL=$(echo "$LATEST_JSON" | grep -o "\"browser_download_url\": *\"[^\"]*${PANEL_ASSET}\"" | head -1 | cut -d'"' -f4)
        if [[ -n "$DL_URL" ]] && curl -fsSL --max-time 90 -L "$DL_URL" -o "$TMP_PANEL/gre-panel" && [[ -s "$TMP_PANEL/gre-panel" ]]; then
            if head -c 4 "$TMP_PANEL/gre-panel" | grep -q "ELF"; then
                DL_OK=1
                echo -e "${GREEN}[✔️] Downloaded prebuilt panel ($(du -h "$TMP_PANEL/gre-panel" | cut -f1)).${NC}"
            else
                echo -e "${YELLOW}[!] Downloaded file is not a binary — falling back to source build.${NC}"
            fi
        fi
        GREPANEL_URL=$(echo "$LATEST_JSON" | grep -o "\"browser_download_url\": *\"[^\"]*grepanel\"" | head -1 | cut -d'"' -f4)
        if [[ -n "$GREPANEL_URL" ]]; then
            curl -fsSL --max-time 30 "$GREPANEL_URL" -o /usr/local/bin/grepanel 2>/dev/null && chmod +x /usr/local/bin/grepanel || true
        fi
        GRESH_URL=$(echo "$LATEST_JSON" | grep -o "\"browser_download_url\": *\"[^\"]*gre\\.sh\"" | head -1 | cut -d'"' -f4)
        if [[ -n "$GRESH_URL" ]]; then
            curl -fsSL --max-time 30 "$GRESH_URL" -o /usr/local/bin/gre.sh 2>/dev/null && chmod +x /usr/local/bin/gre.sh || true
        fi
    fi

    if [[ "$DL_OK" -ne 1 ]]; then
        # fallback: build from source (needs Go)
        echo -e "${YELLOW}[*] No prebuilt panel found — building from source...${NC}"
        if ! command -v go >/dev/null 2>&1; then
            echo -e "${CYAN}[*] Installing Go to build the panel...${NC}"
            apt-get update -qq
            apt-get install -y -qq golang-go
        fi
        if ! curl -fsSL "https://github.com/pdnczone/GRE-FRP/archive/refs/heads/main.tar.gz" -o "$TMP_PANEL/panel.tgz"; then
            echo -e "${RED}[!] Failed to download panel sources.${NC}"
            rm -rf "$TMP_PANEL"
            return 1
        fi
        tar -xzf "$TMP_PANEL/panel.tgz" -C "$TMP_PANEL"
        SRC="$(dirname "$(find "$TMP_PANEL" -name main.go -path '*panel*' | head -1)")"
        if [[ -z "$SRC" || ! -f "$SRC/main.go" ]]; then
            echo -e "${RED}[!] Panel sources not found in archive.${NC}"
            rm -rf "$TMP_PANEL"
            return 1
        fi
        (cd "$SRC" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$TMP_PANEL/gre-panel" .)
        if [[ -f "$SRC/grepanel" ]]; then
            cp "$SRC/grepanel" /usr/local/bin/grepanel
            chmod +x /usr/local/bin/grepanel
        fi
    fi

    cp "$TMP_PANEL/gre-panel" "$PANEL_BIN"
    chmod +x "$PANEL_BIN"
    rm -rf "$TMP_PANEL"

    cat > /etc/systemd/system/gre-panel.service <<EOF
[Unit]
Description=GRE-FRP Web Panel
After=network.target

[Service]
Type=simple
User=root
Restart=always
RestartSec=5s
ExecStart=${PANEL_BIN}

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable gre-panel >/dev/null 2>&1
    systemctl restart gre-panel
    sleep 2

    if systemctl is-active --quiet gre-panel; then
        # fresh password is in the log; save it so option 8 can show it
        NEWPASS=$(journalctl -u gre-panel -n 5 --no-pager 2>/dev/null | grep -o 'panel password: [0-9]*' | tail -1 | awk '{print $3}')
        [[ -n "$NEWPASS" ]] && save_panel_pass "$NEWPASS"
        echo -e "${GREEN}[✔️] Panel installed and running.${NC}"
        # full credentials right here — no need to open another menu
        echo ""
        echo -e "${CYAN}=== Panel credentials ===${NC}"
        show_panel_url
    else
        echo -e "${RED}[!] Panel failed to start — see: journalctl -u gre-panel${NC}"
        return 1
    fi
}

show_panel_url() {
    if [[ ! -f /etc/gre-panel/panel.json ]]; then
        echo -e "${YELLOW}Panel is not installed on this server (no /etc/gre-panel/panel.json). Run Setup first.${NC}"
        return 1
    fi
    local port base user
    port=$(grep -o '"port": *[0-9]*' /etc/gre-panel/panel.json 2>/dev/null | grep -o '[0-9]*')
    base=$(grep -o '"base_path": *"[^"]*"' /etc/gre-panel/panel.json 2>/dev/null | cut -d'"' -f4)
    user=$(grep -o '"username": *"[^"]*"' /etc/gre-panel/panel.json 2>/dev/null | cut -d'"' -f4)
    port=${port:-7777}
    user=${user:-admin}
    ensure_panel_pass
    MYIP=$(ip route get 1.1.1.1 2>/dev/null | awk '/src/ {for (i=1; i<=NF; i++) if ($i=="src") {print $(i+1); exit}}')
    echo -e "${GREEN}Panel URL:  ${CYAN}http://${MYIP:-<this-server-ip>}:${port}/${base}${NC}"
    echo -e "${GREEN}Username:   ${CYAN}${user}${NC}"
    echo -e "${GREEN}Password:   ${CYAN}${PANEL_PASS}${NC}"
}

# make sure a plaintext password exists and load it into $PANEL_PASS.
# fresh installs already have it (binary writes it); old installs get a new one.
ensure_panel_pass() {
    PANEL_PASS=$(cat /etc/gre-panel/panel.pass 2>/dev/null)
    if [[ -n "$PANEL_PASS" ]]; then return 0; fi
    echo -e "${YELLOW}[*] No saved panel password — generating a new one...${NC}"
    local NEWPASS HASH
    NEWPASS=$(tr -dc '0-9' </dev/urandom | head -c 8)
    HASH=$(echo -n "$NEWPASS" | sha256sum | awk '{print $1}')
    if [[ -z "$HASH" ]] || ! command -v python3 >/dev/null 2>&1; then
        echo -e "${RED}[!] Cannot reset password (need sha256sum + python3). Change it from web Settings instead.${NC}"
        PANEL_PASS="(unknown — reset via web Settings)"
        return 1
    fi
    python3 - "$HASH" <<'PYEOF'
import json, sys
p = '/etc/gre-panel/panel.json'
d = json.load(open(p))
d['pass_hash'] = sys.argv[1]
json.dump(d, open(p, 'w'), indent=2)
PYEOF
    echo -n "$NEWPASS" > /etc/gre-panel/panel.pass
    chmod 600 /etc/gre-panel/panel.pass
    systemctl restart gre-panel 2>/dev/null
    sleep 2
    PANEL_PASS="$NEWPASS"
    return 0
}

# save plaintext panel password next to config (user chose convenience over max security)
save_panel_pass() {
    local pass="$1"
    [[ -n "$pass" ]] && echo -n "$pass" > /etc/gre-panel/panel.pass 2>/dev/null
    chmod 600 /etc/gre-panel/panel.pass 2>/dev/null || true
}

update_all() {
    echo -e "${CYAN}[*] Updating GRE-FRP (script + panel binary)...${NC}"
    TMP_U="$(mktemp -d)"
    trap 'rm -rf "$TMP_U"' RETURN
    # 1. fresh script from main
    if ! curl -fsSL --max-time 30 "https://raw.githubusercontent.com/pdnczone/GRE-FRP/main/gre.sh" -o "$TMP_U/gre.sh"; then
        echo -e "${RED}[!] Failed to download latest gre.sh — nothing changed.${NC}"
        return 1
    fi
    bash -n "$TMP_U/gre.sh" || { echo -e "${RED}[!] Downloaded script failed syntax check — nothing changed.${NC}"; return 1; }
    if cmp -s "$TMP_U/gre.sh" "$0" 2>/dev/null || cmp -s "$TMP_U/gre.sh" ./gre.sh 2>/dev/null; then
        echo -e "${GREEN}[✔️] gre.sh is already the latest version.${NC}"
    else
        echo -e "${GREEN}[✔️] New gre.sh downloaded and syntax-checked.${NC}"
    fi
    # 2. reinstall panel binary from latest release (downloads prebuilt, restarts service)
    echo -e "${CYAN}[*] Updating panel binary...${NC}"
    # backup panel config so a failed update can be rolled back
    PANEL_BAK=""
    if [[ -f /etc/gre-panel/panel.json ]]; then
        PANEL_BAK="$(mktemp -d)"
        cp -a /etc/gre-panel/panel.json /etc/gre-panel/panel.pass "$PANEL_BAK/" 2>/dev/null || true
    fi
    if ! install_panel; then
        echo -e "${RED}[!] Panel update failed — restoring previous config.${NC}"
        [[ -n "$PANEL_BAK" ]] && cp -a "$PANEL_BAK/panel.json" "$PANEL_BAK/panel.pass" /etc/gre-panel/ 2>/dev/null || true
        systemctl restart gre-panel 2>/dev/null || true
        return 1
    fi
    [[ -n "$PANEL_BAK" ]] && rm -rf "$PANEL_BAK"
    # 3. replace running script only after everything succeeded
    cp "$TMP_U/gre.sh" "$0" 2>/dev/null || cp "$TMP_U/gre.sh" ./gre.sh
    chmod +x "$0" 2>/dev/null || true
    # 4. sync a copy next to the panel binary so the web panel + grepanel
    # always shell out to the latest tune/setup logic (single source of truth)
    cp "$TMP_U/gre.sh" /usr/local/bin/gre.sh 2>/dev/null && chmod +x /usr/local/bin/gre.sh || true
    PANEL_VER=$("$PANEL_BIN" --version 2>/dev/null || echo "unknown")
    echo -e "${GREEN}[✔️] Update complete — script + panel are latest (panel: ${PANEL_VER}). Re-run the script to use the new menu.${NC}"
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
    echo "7) Update All (latest script + latest panel binary)"
    echo "8) Show Panel URL + Username + Password"
    echo "9) Remove Tunnel (GRE + FRP, panel stays)"
    echo "10) Optimize Tunnel (BBR + buffers + MTU/MSS, with backup)"
    echo "11) Restore Pre-Optimize Settings"
    echo "12) Optimization Status"
    echo "0) Exit"
    echo ""
    read -p "Select an option [0-12]: " OPTION

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
        7)
            update_all
            ;;
        8)
            show_panel_url
            ;;
        9)
            remove_tunnel
            ;;
        10)
            tune_apply
            ;;
        11)
            tune_restore
            ;;
        12)
            tune_status
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
# Non-interactive CLI: gre.sh setup-iran|setup-foreign with flags.
# The setup_*_noninteractive + _setup_foreign_full functions above are the
# SINGLE source of truth — menu, CLI, and web panel all run the same steps.
usage_cli() {
    cat <<EOF
Usage:
  bash gre.sh                                   # interactive menu
  bash gre.sh setup-iran    --local-pub IP --remote-pub IP [--frp-port N] [--local-gre IP] [--peer-gre IP] [--token T] [--force]
  bash gre.sh setup-foreign --local-pub IP --remote-pub IP [--frp-port N] --token T --ports "443, 2083" [--local-gre IP] [--peer-gre IP] [--force]
  bash gre.sh status | remove-tunnel [--force] | show-panel-url
  bash gre.sh optimize | restore | tune-status
EOF
}

cli_setup_iran() {
    local LOCAL_PUB="" REMOTE_PUB="" FRP_PORT="7000" LOCAL_GRE="$IRAN_GRE_IP" PEER_GRE="$FOREIGN_GRE_IP" TOKEN="" FORCE=0
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --local-pub) LOCAL_PUB="$2"; shift 2 ;;
            --remote-pub) REMOTE_PUB="$2"; shift 2 ;;
            --frp-port) FRP_PORT="$2"; shift 2 ;;
            --local-gre) LOCAL_GRE="$2"; shift 2 ;;
            --peer-gre) PEER_GRE="$2"; shift 2 ;;
            --token) TOKEN="$2"; shift 2 ;;
            --force) FORCE=1; shift ;;
            -h|--help) usage_cli; return 0 ;;
            *) echo -e "${RED}[!] Unknown flag: $1${NC}"; usage_cli; return 1 ;;
        esac
    done
    validate_setup_common "$LOCAL_PUB" "$REMOTE_PUB" "$FRP_PORT" "$LOCAL_GRE" || return 1
    is_valid_ip "$PEER_GRE" || { echo -e "${RED}[!] Invalid peer GRE IP: '$PEER_GRE'${NC}"; return 1; }
    if [[ -z "$TOKEN" ]]; then
        TOKEN=$(tr -dc A-Za-z0-9 </dev/urandom | head -c 16 2>/dev/null || openssl rand -hex 8)
        echo -e "${CYAN}[*] Generated token: ${TOKEN}${NC}"
    fi
    if tunnel_present && [[ "$FORCE" -ne 1 ]]; then
        echo -e "${RED}[!] Tunnel already exists — pass --force to overwrite.${NC}"
        return 1
    fi
    setup_iran_server_noninteractive "$LOCAL_PUB" "$REMOTE_PUB" "$FRP_PORT" "$TOKEN" "$LOCAL_GRE" "$PEER_GRE"
}

cli_setup_foreign() {
    local LOCAL_PUB="" REMOTE_PUB="" FRP_PORT="7000" LOCAL_GRE="$FOREIGN_GRE_IP" PEER_GRE="$IRAN_GRE_IP" TOKEN="" PORTS="" FORCE=0
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --local-pub) LOCAL_PUB="$2"; shift 2 ;;
            --remote-pub) REMOTE_PUB="$2"; shift 2 ;;
            --frp-port) FRP_PORT="$2"; shift 2 ;;
            --local-gre) LOCAL_GRE="$2"; shift 2 ;;
            --peer-gre) PEER_GRE="$2"; shift 2 ;;
            --token) TOKEN="$2"; shift 2 ;;
            --ports) PORTS="$2"; shift 2 ;;
            --force) FORCE=1; shift ;;
            -h|--help) usage_cli; return 0 ;;
            *) echo -e "${RED}[!] Unknown flag: $1${NC}"; usage_cli; return 1 ;;
        esac
    done
    validate_setup_common "$LOCAL_PUB" "$REMOTE_PUB" "$FRP_PORT" "$LOCAL_GRE" || return 1
    is_valid_ip "$PEER_GRE" || { echo -e "${RED}[!] Invalid peer GRE IP: '$PEER_GRE'${NC}"; return 1; }
    [[ -n "$TOKEN" ]] || { echo -e "${RED}[!] --token is required (copy it from the Iran side).${NC}"; return 1; }
    local CLEANED="" p
    for p in $(echo "$PORTS" | tr ',' ' '); do
        is_valid_port "$p" && CLEANED="$CLEANED $((10#$p))"
    done
    CLEANED=$(echo "$CLEANED" | xargs)
    [[ -n "$CLEANED" ]] || { echo -e "${RED}[!] --ports needs at least one valid port (e.g. \"443, 2083\").${NC}"; return 1; }
    if tunnel_present && [[ "$FORCE" -ne 1 ]]; then
        echo -e "${RED}[!] Tunnel already exists — pass --force to overwrite.${NC}"
        return 1
    fi
    setup_foreign_server_noninteractive "$LOCAL_PUB" "$REMOTE_PUB" "$FRP_PORT" "$TOKEN" "$LOCAL_GRE" "$PEER_GRE" "$CLEANED"
}

if [[ $# -gt 0 ]]; then
    check_root
    case "$1" in
        setup-iran) shift; cli_setup_iran "$@" ;;
        setup-foreign) shift; cli_setup_foreign "$@" ;;
        status) check_status ;;
        optimize) tune_apply ;;
        restore) tune_restore ;;
        tune-status) tune_status ;;
        remove-tunnel)
            if [[ "${2:-}" == "--force" ]]; then remove_tunnel_force; else remove_tunnel; fi ;;
        show-panel-url) show_panel_url ;;
        -h|--help|help) usage_cli ;;
        *) echo -e "${RED}[!] Unknown command: $1${NC}"; usage_cli; exit 1 ;;
    esac
    exit $?
fi
main_menu
