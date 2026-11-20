#!/bin/bash
set -euo pipefail

if [[ ${1:-} != desktop ]]; then
    exec "$@"
fi

mode=${AGENTD_MODE:-desktop}
case "$mode" in
    desktop | telegram) ;;
    *) echo "AGENTD_MODE must be desktop or telegram" >&2; exit 1 ;;
esac
if [[ $mode == telegram && ! -f $AGENTD_HOME/config.yaml ]]; then
    echo 'Run agentd setup before enabling Telegram mode.' >&2
    exit 1
fi

wait_until_ready() {
    for ((attempt = 0; attempt < 30; attempt++)); do
        if "$@" >/dev/null 2>&1; then
            return
        fi
        sleep 1
    done
    echo "Timed out waiting for: $*" >&2
    return 1
}

desktop_pids=()
browser_pid=
telegram_pid=
cleanup() {
    trap '' INT TERM
    if [[ -n $telegram_pid ]]; then
        kill -TERM "$telegram_pid" 2>/dev/null || true
        wait "$telegram_pid" 2>/dev/null || true
    fi
    if [[ -n $browser_pid ]]; then
        kill -TERM "$browser_pid" 2>/dev/null || true
        wait "$browser_pid" 2>/dev/null || true
    fi
    if ((${#desktop_pids[@]})); then
        kill -TERM "${desktop_pids[@]}" 2>/dev/null || true
        wait "${desktop_pids[@]}" 2>/dev/null || true
    fi
}
trap cleanup EXIT
trap 'exit 0' INT TERM

install -d -m 0700 "$XDG_RUNTIME_DIR"
# Remove sockets left by a forced stop of this container.
rm -f "$XDG_RUNTIME_DIR/bus" /tmp/.X0-lock /tmp/.X11-unix/X0
install -m 0600 /dev/null "$XAUTHORITY"
xauth add "$DISPLAY" . "$(mcookie)"

Xvfb "$DISPLAY" -screen 0 1440x900x24 -nolisten tcp -auth "$XAUTHORITY" &
desktop_pids+=("$!")
dbus-daemon --session --nofork --nopidfile --address="$DBUS_SESSION_BUS_ADDRESS" &
desktop_pids+=("$!")
wait_until_ready xdpyinfo
wait_until_ready dbus-send --session --print-reply --reply-timeout=1000 \
    --dest=org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus.ListNames

xfce4-session &
desktop_pids+=("$!")
x11vnc -display "$DISPLAY" -localhost -rfbport 5900 -nopw -forever -shared &
desktop_pids+=("$!")
websockify --web /usr/share/novnc 0.0.0.0:6080 127.0.0.1:5900 &
desktop_pids+=("$!")
chromium --user-data-dir="$AGENTD_HOME/browser" --remote-debugging-port=9222 \
    --no-first-run --no-default-browser-check --password-store=basic --no-sandbox about:blank &
browser_pid=$!
wait_until_ready curl -fsS --noproxy '*' --max-time 1 http://127.0.0.1:9222/json/version

if [[ $mode == telegram ]]; then
    agentd -config "$AGENTD_HOME/config.yaml" -telegram &
    telegram_pid=$!
fi

wait -n
echo 'A desktop or gateway process exited.' >&2
exit 1
