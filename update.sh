#!/usr/bin/env bash
# ARTEX update script: ① Docker update (pull the new image and rebuild)  ② local build update (rebuild the binary)
# Mirrors install.sh: install is responsible for the first deployment, update is responsible for upgrading to a new version.
# No manual DB migration is needed — artex re-runs schema.sql idempotently on every startup (including ADD COLUMN/CREATE
# INDEX IF NOT EXISTS), so "restart to migrate". Data (pgdata volume, ./data, ./skills) is unaffected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── optional: sync the repo to the latest code (compose/scripts/local build source all update via it) ───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "not a git working copy, skipping git pull"; return; }
  [ "$(ask 'pull the latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or divergent branches) — please handle manually and retry; this run keeps the current code"
  fi
}

# ── ① Docker update ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not detected; please first deploy with ./install.sh"
  [ -f .env ] || die ".env not found; please first run ./install.sh to complete the initial deployment"

  # optional: upgrade to a specified version tag (if left empty, keeps ARTEX_TAG from .env, default latest)
  local tag; tag="$(ask 'target image tag (press Enter to keep .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG set to ${tag}"
  fi

  # Only touch artex: postgres is pinned to 16-alpine and need not follow upgrades (pulling it is pure
  # bandwidth waste, and major-version changes carry compatibility risk). artex declares
  # depends_on postgres, so bringing it up by service name auto-starts pg if it's not running;
  # already-running pg is kept as-is and not rebuilt.
  info "pulling the new image (artex only)…"
  docker compose pull artex
  info "rebuilding and starting (artex auto-migrates the schema on restart)…"
  docker compose up -d artex
  ok "update complete → http://localhost:8787"
  info "view logs: docker compose logs -f artex"
  info "clean up old images (optional): docker image prune -f"
}

# ── ② local build update ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go (>=1.26) not detected: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found — for a first deployment, use ./install.sh instead"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "rebuilding frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "recompiling the embedded single binary…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not detected: compiling the backend **without the embedded frontend** (run npm run dev separately for the frontend)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "build complete → ./artex"
  warn "please restart the running artex process to take effect (the schema auto-migrates on restart)"
}

echo "=============================="
echo "  ARTEX Update"
echo "  1) Docker update (pull new image and rebuild)"
echo "  2) Local update (rebuild with go)"
echo "=============================="
case "$(ask 'choose' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "invalid choice" ;;
esac
