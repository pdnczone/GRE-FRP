#!/usr/bin/env bash
# Hashem one-line installer — downloads gre.sh and runs it.
# Usage: bash <(curl -fsSL https://raw.githubusercontent.com/pdnczone/hashem/main/install.sh)
set -euo pipefail
if [[ $EUID -ne 0 ]]; then echo "Please run as root (sudo)."; exit 1; fi
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
curl -fsSL https://raw.githubusercontent.com/pdnczone/hashem/main/gre.sh -o "$TMP/gre.sh"
chmod +x "$TMP/gre.sh"
exec bash "$TMP/gre.sh"
