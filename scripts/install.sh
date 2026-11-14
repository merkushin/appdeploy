#!/usr/bin/env bash
# Install appdeploy on a Debian/Ubuntu server that runs systemd. Run as root.
#
#   ./install.sh --binary ./appdeploy-linux-amd64 --ssh-key-file ~/.ssh/id_ed25519.pub
#
# What it does (and nothing else):
#   - installs git, Docker (get.docker.com) and Caddy (the official apt repository)
#   - installs the appdeploy binary as /usr/local/bin/appdeploy
#   - creates the unix user "deploy" whose only SSH capability is `appdeploy shell`
#   - installs and starts two systemd services: appdeployd (the agent) and
#     appdeploy-caddy (Caddy driven by the agent)
#
# It is safe to run again: every step checks before it changes anything.
# It does not touch the firewall or sshd_config; see the notes printed at the end.
set -euo pipefail

BINARY=""
SSH_KEYS=()       # admin: may do everything
DEPLOY_KEYS=()    # deployer: git push, deploy, rollback, logs
VIEW_KEYS=()      # viewer: status and lists only

usage() {
  sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BINARY="${2:?--binary needs a path}"; shift 2 ;;
    --ssh-key) SSH_KEYS+=("${2:?--ssh-key needs a public key}"); shift 2 ;;
    --deployer-key) DEPLOY_KEYS+=("${2:?--deployer-key needs a public key}"); shift 2 ;;
    --viewer-key) VIEW_KEYS+=("${2:?--viewer-key needs a public key}"); shift 2 ;;
    --ssh-key-file)
      while IFS= read -r line; do [ -n "$line" ] && SSH_KEYS+=("$line"); done < "${2:?--ssh-key-file needs a path}"
      shift 2 ;;
    -h|--help) usage 0 ;;
    *) echo "unknown option: $1" >&2; usage 2 ;;
  esac
done

log() { printf '==> %s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root"
[ -n "$BINARY" ] || die "--binary <path to the linux appdeploy binary> is required"
[ -f "$BINARY" ] || die "no such file: $BINARY"
[ "${#SSH_KEYS[@]}" -gt 0 ] || die "give at least one admin key with --ssh-key or --ssh-key-file"
command -v systemctl >/dev/null || die "systemd is required"
command -v apt-get >/dev/null || die "only Debian/Ubuntu (apt) is supported for now"

export DEBIAN_FRONTEND=noninteractive

log "packages: git, curl, gnupg, tar"
apt-get update -qq
apt-get install -y -qq git curl gnupg tar ca-certificates apt-transport-https debian-archive-keyring >/dev/null 2>&1 \
  || apt-get install -y -qq git curl gnupg tar ca-certificates apt-transport-https >/dev/null

if ! command -v docker >/dev/null; then
  log "installing Docker with the official convenience script"
  curl -fsSL https://get.docker.com | sh
fi
# Docker's default address pool holds about 30 networks and every app gets one;
# hand it a pool of /24s instead (unless the admin already configured Docker).
if [ ! -s /etc/docker/daemon.json ]; then
  mkdir -p /etc/docker
  cat > /etc/docker/daemon.json <<'JSON'
{"default-address-pools": [{"base": "172.80.0.0/12", "size": 24}]}
JSON
  systemctl restart docker 2>/dev/null || true
fi
systemctl enable --now docker

if ! command -v caddy >/dev/null; then
  log "installing Caddy from the official apt repository"
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq
  apt-get install -y -qq caddy
fi
# The packaged service is replaced by appdeploy-caddy below.
systemctl disable --now caddy >/dev/null 2>&1 || true

log "installing /usr/local/bin/appdeploy"
install -m 0755 "$BINARY" /usr/local/bin/appdeploy.new
mv /usr/local/bin/appdeploy.new /usr/local/bin/appdeploy

log "user deploy (SSH forced command: appdeploy shell)"
getent group appdeploy >/dev/null || groupadd --system appdeploy
if ! id deploy >/dev/null 2>&1; then
  useradd --system --create-home --home-dir /home/deploy --shell /bin/sh --gid appdeploy deploy
fi
usermod -g appdeploy deploy
# An account with "*" as its password is not locked (sshd accepts keys) but has no password.
usermod -p '*' deploy
install -d -m 0700 -o deploy -g appdeploy /home/deploy/.ssh
AUTH=/home/deploy/.ssh/authorized_keys
: > "$AUTH.new"
write_keys() { # write_keys <role> <keys...>
  local role="$1"; shift
  local key
  for key in "$@"; do
    printf 'command="/usr/local/bin/appdeploy shell --role %s",no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty %s\n' "$role" "$key" >> "$AUTH.new"
  done
}
write_keys admin "${SSH_KEYS[@]}"
[ "${#DEPLOY_KEYS[@]}" -eq 0 ] || write_keys deployer "${DEPLOY_KEYS[@]}"
[ "${#VIEW_KEYS[@]}" -eq 0 ] || write_keys viewer "${VIEW_KEYS[@]}"
chown deploy:appdeploy "$AUTH.new"
chmod 0600 "$AUTH.new"
mv "$AUTH.new" "$AUTH"

log "directories"
install -d -m 0755 /etc/appdeploy /var/lib/appdeploy
id caddy >/dev/null 2>&1 || die "the caddy user is missing (Caddy package not installed?)"
install -d -m 0755 -o caddy -g caddy /var/lib/appdeploy/caddy /var/log/appdeploy
cat > /etc/appdeploy/caddy.json <<'JSON'
{"admin": {"listen": "127.0.0.1:2019"}}
JSON

log "systemd units"
cat > /etc/systemd/system/appdeploy-caddy.service <<'UNIT'
[Unit]
Description=Caddy proxy for appdeploy
After=network-online.target
Wants=network-online.target

[Service]
User=caddy
Group=caddy
Environment=XDG_CONFIG_HOME=/var/lib/appdeploy/caddy/config
Environment=XDG_DATA_HOME=/var/lib/appdeploy/caddy/data
# --resume starts from the last configuration the agent loaded, so apps are
# served after a reboot even before the agent is up.
ExecStart=/usr/bin/caddy run --resume --config /etc/appdeploy/caddy.json
Restart=on-failure
RestartSec=2
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
UNIT

cat > /etc/systemd/system/appdeployd.service <<'UNIT'
[Unit]
Description=appdeploy agent
After=network-online.target docker.service appdeploy-caddy.service
Requires=docker.service
Wants=appdeploy-caddy.service

[Service]
# --access-log: Caddy writes it, the agent follows it for the rollback watchdog.
ExecStart=/usr/local/bin/appdeploy agent --data-dir /var/lib/appdeploy --socket /run/appdeploy/agent.sock --socket-group appdeploy --repo-owner deploy --access-log /var/log/appdeploy/access.log --prune-build-cache 168h --health-listen :7071
RuntimeDirectory=appdeploy
RuntimeDirectoryMode=0755
Restart=always
RestartSec=2
# Containers belong to Docker, not to the agent: restarting the agent leaves
# them running, and unfinished deploys resume when it comes back.
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now appdeploy-caddy.service
systemctl enable appdeployd.service
systemctl restart appdeployd.service

log "waiting for the agent"
for _ in $(seq 1 30); do
  if /usr/local/bin/appdeploy --socket /run/appdeploy/agent.sock ping >/dev/null 2>&1; then
    log "appdeploy is running"
    cat <<'EOF'

Next steps
  1. Point your DNS names at this server (ports 80 and 443 must be reachable
     for Let's Encrypt).
  2. From your laptop:
       appdeploy --host deploy@<server> apps create myapp --domain myapp.example.com
       git remote add prod deploy@<server>:myapp
       git push prod main
  3. Firewall: this script does not change it. Allow 22, 80 and 443 only.
  4. Keys given with --ssh-key are admins (everything, including exec and secrets).
     --deployer-key may git push, deploy, roll back and read logs; --viewer-key
     only sees status and lists. The role is set per key in
     /home/deploy/.ssh/authorized_keys.
EOF
    exit 0
  fi
  sleep 1
done
journalctl -u appdeployd -n 30 --no-pager >&2 || true
die "the agent did not come up; see the log above"
