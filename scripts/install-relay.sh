#!/bin/sh
# Installs a LANBridge relay as a systemd service on a Linux server (x64 or arm64).
#   curl -fsSL https://github.com/__REPO__/releases/latest/download/install-relay.sh | sudo sh
# Optional: LANBRIDGE_PORT (default 7777), LANBRIDGE_RELAY_TOKEN (default: random).
set -eu
REPO="${LANBRIDGE_REPO:-__REPO__}"
PORT="${LANBRIDGE_PORT:-7777}"
[ "$(id -u)" -eq 0 ] || { echo "Run this with sudo." >&2; exit 1; }
case "$(uname -m)" in
  x86_64 | amd64) arch=x64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "Unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac
tmp="$(mktemp -d)"
curl -fsSL "https://github.com/$REPO/releases/latest/download/lanbridge-linux-$arch.tar.gz" | tar -xz -C "$tmp"
install -m 0755 "$tmp/lanbridge" /usr/local/bin/lanbridge
rm -rf "$tmp"
TOKEN="${LANBRIDGE_RELAY_TOKEN:-$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-32)}"
cat >/etc/systemd/system/lanbridge-relay.service <<EOF
[Unit]
Description=LANBridge relay
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/lanbridge relay --listen :$PORT
Environment=LANBRIDGE_RELAY_TOKEN=$TOKEN
DynamicUser=yes
NoNewPrivileges=yes
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now lanbridge-relay
systemctl restart lanbridge-relay
# Open the port in common host firewalls. Cloud firewalls (security lists) still need it too.
if command -v ufw >/dev/null 2>&1 && ufw status | grep -q "Status: active"; then ufw allow "$PORT/tcp" >/dev/null; fi
if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
  firewall-cmd --permanent --add-port="$PORT/tcp" >/dev/null && firewall-cmd --reload >/dev/null
fi
if command -v iptables >/dev/null 2>&1 && iptables -S INPUT 2>/dev/null | grep -q -- "-j REJECT"; then
  iptables -I INPUT -p tcp --dport "$PORT" -j ACCEPT # e.g. Oracle Cloud's default Ubuntu rules
  command -v netfilter-persistent >/dev/null 2>&1 && netfilter-persistent save >/dev/null 2>&1 || true
fi
IP="$(curl -fsS4 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')"
echo
echo "LANBridge relay is running on port $PORT."
echo "Hosts put this in LANBridge > Settings (friends who join don't need it):"
echo "  Relay address: tcp://$IP:$PORT"
echo "  Relay token:   $TOKEN"
echo "If your cloud provider has a firewall or security list, allow TCP port $PORT there too."
