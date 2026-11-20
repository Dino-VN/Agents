#!/bin/sh
# Build agentd for the server and swap the binary in, retrying over a flaky link.
# Usage: scripts/deploy.sh [user@host]   (default root@192.168.1.195)
set -eu
HOST="${1:-root@192.168.1.195}"
SSH="ssh -o BatchMode=yes -o ConnectTimeout=30 -o ServerAliveInterval=10 -o ServerAliveCountMax=6"
cd "$(dirname "$0")/.."

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/agentd-linux-arm64 ./cmd/agentd
gzip -9 -c bin/agentd-linux-arm64 > bin/agentd-linux-arm64.gz
SUM=$(shasum -a 256 bin/agentd-linux-arm64 | cut -d' ' -f1)

try() { n=0; until "$@"; do n=$((n + 1)); [ $n -ge 5 ] && return 1; echo "retry $n…" >&2; sleep 10; done; }

# Upload to a temp file and only swap it in when the checksum matches.
try sh -c "$SSH $HOST 'gunzip -c > /usr/local/bin/agentd.new && chmod 755 /usr/local/bin/agentd.new' < bin/agentd-linux-arm64.gz"
try $SSH "$HOST" "test \"\$(sha256sum /usr/local/bin/agentd.new | cut -d' ' -f1)\" = $SUM \
  && mv /usr/local/bin/agentd.new /usr/local/bin/agentd \
  && rc-service agentd restart >/dev/null && sleep 5 && rc-service agentd status"
rm -f bin/agentd-linux-arm64.gz
echo "deployed $SUM"
