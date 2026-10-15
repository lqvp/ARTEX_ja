# syntax=docker/dockerfile:1
#
# Multi-stage build: frontend static export → Go single binary (embedded frontend) → runtime image.
# The original Docker Hub image source (autumn27/artex) is no longer maintained; this repo now
# supports self-service building from pure source:
#   docker compose build        # or docker build -t artex:local .
# TARGETARCH is injected automatically by buildx (amd64/arm64); Go cross-compilation needs no QEMU.
# The runtime layer installs only tools; all compilation happens in the build stages.
# Behind mainland-China networks the module sources can be overridden (defaults are already
# China-friendly):
#   --build-arg GOPROXY=https://goproxy.cn,direct
#   --build-arg NPM_REGISTRY=https://registry.npmmirror.com

########## Stage 1: frontend static export ##########
FROM node:20-bookworm AS web
ARG NPM_REGISTRY=https://registry.npmmirror.com
WORKDIR /web
# --ignore-scripts: skips the prepare(husky) hook — there is no .git in the container and husky
# would fail; Next.js/Biome platform binaries come via optionalDependencies and don't rely on
# lifecycle scripts.
COPY web/package.json web/package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci --ignore-scripts
COPY web/ ./
RUN NEXT_EXPORT=1 npm run build:static

########## Stage 2: Go backend (embedded frontend, static build) ##########
FROM golang:1.26-bookworm AS gobuild
ARG TARGETARCH
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Frontend static artifacts are copied in from the web stage (.dockerignore already excludes
# the host dist/ directory, so there is no conflict).
COPY --from=web /web/out/ server/webui/dist/
RUN CGO_ENABLED=0 GOARCH="${TARGETARCH}" go build -tags embedui -trimpath -o /out/artex ./cmd/artex

########## Stage 3: runtime image (same as the original: tools + preinstalled Playwright) ##########
FROM python:3.12-slim-bookworm
ARG NPM_REGISTRY=https://registry.npmmirror.com
# The runtime layer's global npm installs and the chromium download also use China mirrors
# (reachable from global networks too).
ENV npm_config_registry=$NPM_REGISTRY \
    PLAYWRIGHT_DOWNLOAD_HOST=https://cdn.npmmirror.com/binaries/playwright
# Common tools: ripgrep / curl / vim, plus a batch of recon staples (add/remove as needed).
# Node 20.x from NodeSource: bookworm's bundled apt nodejs is 18, and Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP and CLI (globally) so runtime no longer downloads via npx.
# @playwright/mcp: browser MCP directly via `npx @playwright/mcp` (globally installed, no -y/@latest needed).
# @playwright/cli: provides playwright-cli; after install verify with --help.
# Then install playwright (provides browser management); use --with-deps after install to
# preset chromium and its system dependencies, so the container's MCP/CLI works on first
# startup without downloading a browser over the network.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# The single binary produced by the build stages (embedded frontend)
COPY --from=gobuild /out/artex /app/artex
# Daemon start script: after the process exits it decides whether to pull it back up based on
# the exit code; the page's one-click update relies on it to complete the swap.
# It is also responsible for forwarding SIGTERM to artex — docker stop only sends the signal
# to PID 1; without forwarding artex receives nothing, cannot do a graceful shutdown, and
# gets SIGKILLed after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
