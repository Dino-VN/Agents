FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY assets.go ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY roles/ ./roles/
COPY skills/ ./skills/
COPY souls/ ./souls/

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags='-s -w' -o /out/agentd ./cmd/agentd

RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) cua_arch=x86_64; sha=1cb8aa6d30e5cdf5bf4d222cb3693a50af2c178073a49dc59e88737c5f7807e6 ;; \
      arm64) cua_arch=arm64; sha=401f7f4a9ad276dbcdb324f1b00e152987a46985b68c9e625ac01101f5e6af77 ;; \
      *) echo "Unsupported cua-driver architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 3 -o /tmp/cua-driver.tar.gz \
      "https://github.com/trycua/cua/releases/download/cua-driver-rs-v0.55.0/cua-driver-rs-0.55.0-linux-$cua_arch-binary.tar.gz"; \
    echo "$sha  /tmp/cua-driver.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/cua-driver.tar.gz -C /out cua-driver

FROM debian:trixie-slim

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      at-spi2-core ca-certificates chromium curl dbus-x11 fonts-liberation git \
      libwayland-client0 libx11-6 libxext6 libxi6 libxkbcommon0 libxtst6 \
      mousepad novnc thunar websockify x11-utils x11vnc xauth xvfb \
      xfce4-panel xfce4-session xfce4-terminal xfdesktop4 xfwm4 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 --user-group --shell /bin/bash agentd \
    && chmod 0700 /home/agentd \
    && install -d -m 0700 -o agentd -g agentd /home/agentd/.agentd /home/agentd/workspace \
    && install -d -m 1777 /tmp/.X11-unix

COPY --from=build /out/agentd /out/cua-driver /usr/local/bin/
COPY --chmod=755 docker/desktop.sh /usr/local/bin/agentd-desktop
RUN cua-driver --version

ENV HOME=/home/agentd \
    AGENTD_HOME=/home/agentd/.agentd \
    AGENTD_BROWSER_CDP=http://127.0.0.1:9222 \
    AGENTD_CUA_DRIVER=cua-driver \
    DISPLAY=:0 \
    XDG_SESSION_TYPE=x11 \
    XDG_RUNTIME_DIR=/tmp/agentd-runtime \
    XAUTHORITY=/tmp/agentd-runtime/Xauthority \
    DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/agentd-runtime/bus

USER agentd
WORKDIR /home/agentd/workspace
EXPOSE 6080
ENTRYPOINT ["/usr/local/bin/agentd-desktop"]
CMD ["desktop"]
