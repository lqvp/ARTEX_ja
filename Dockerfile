# syntax=docker/dockerfile:1
#
# Runtime image (no compilation in the image): installs only common tools and places the
# **pre-compiled Linux single binary** in it. The binary is cross-compiled by the CI's
# binaries job (pure Go, no QEMU), placed in the build context at dist/<TARGETARCH>/artex
# per target architecture. This way multi-arch builds only need to emulate the apt layer
# for arm64, no longer emulating Next/Go compilation, which is much faster.
#
# For local manual image builds, prepare the binary first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
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
# Pre-compiled binary for the corresponding architecture (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# Daemon start script: after the process exits it decides whether to pull it back up based on
# the exit code; the page's one-click update relies on it to complete the swap.
# It is also responsible for forwarding SIGTERM to artex — docker stop only sends the signal
# to PID 1; without forwarding artex receives nothing, cannot do a graceful shutdown, and
# gets SIGKILLed after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /App/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
