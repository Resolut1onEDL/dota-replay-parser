#!/bin/bash
# release-sync.sh vX.Y.Z — ONE release train for every parser consumer.
#
# The parser has exactly one source (this repo) but several delivery
# channels, and history proved they drift silently (prod once held a mix of
# 4.3.1..4.4.4 rows arriving simultaneously). After a release tag exists,
# this script fans the version out to every channel and VERIFIES each one:
#
#   1. GitHub release assets (built by CI on the tag)   — checked
#   2. Fly parse-service `resoai-parse` (web + ODB)     — deployed + healthz
#   3. reso-coach-companion pin + bin/                  — bumped + checksum
#   4. gamerjournal-replay-uploader pin + bin/ (legacy) — bumped + checksum
#   5. local GJ binary (DOTA_PARSER_BIN)                — rebuilt + --version
#
# Steps that MUST stay human (release policy): creating the tag, and
# publishing the companion/uploader releases (electron auto-update). The
# script prints those commands instead of running them.
#
# Env overrides: PARSER_REPO, COMPANION_DIR, UPLOADER_DIR, PROJECTS_DIR.
set -euo pipefail

V="${1:-}"
[ -n "$V" ] || { echo "usage: scripts/release-sync.sh vX.Y.Z"; exit 1; }
case "$V" in v*) ;; *) V="v$V";; esac
VER="${V#v}"

PROJECTS_DIR="${PROJECTS_DIR:-$HOME/Projects}"
PARSER_REPO="${PARSER_REPO:-$PROJECTS_DIR/dota-replay-parser}"
COMPANION_DIR="${COMPANION_DIR:-$PROJECTS_DIR/reso-coach-companion}"
UPLOADER_DIR="${UPLOADER_DIR:-$PROJECTS_DIR/gamerjournal-replay-uploader}"
REPO_SLUG="Resolut1onEDL/dota-replay-parser"
FLY_APP="resoai-parse"

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL:\033[0m %s\n' "$*"; exit 1; }

step "0/5 tag + CI assets ($V)"
if ! git -C "$PARSER_REPO" rev-parse "$V" >/dev/null 2>&1 &&
   ! gh release view "$V" -R "$REPO_SLUG" >/dev/null 2>&1; then
  echo "Тег $V не существует. Поставь его сам (релизы — ручное действие):"
  echo "  cd $PARSER_REPO && git tag $V && git push origin $V"
  exit 1
fi
# CI builds the three assets on the tag — poll until they are all there.
for i in $(seq 1 30); do
  n=$(gh release view "$V" -R "$REPO_SLUG" --json assets \
      --jq '[.assets[].name] | map(select(. == "parser-mac-arm64" or . == "parser-linux-x64" or . == "parser-win-x64.exe")) | length' 2>/dev/null || echo 0)
  [ "$n" = "3" ] && break
  echo "  assets: $n/3 — жду CI (${i}0s)"; sleep 10
done
[ "$n" = "3" ] || fail "release $V не содержит трёх бинарей (CI не добежал?)"
echo "  release $V: 3/3 asset'а на месте"

step "1/5 Fly $FLY_APP (веб + ODB)"
(cd "$PARSER_REPO" && git fetch -q origin && git -C . stash list >/dev/null; fly deploy --remote-only -a "$FLY_APP")
for i in $(seq 1 12); do
  got=$(curl -sf -m 15 "https://$FLY_APP.fly.dev/healthz" | sed -n 's/.*"parser":"\([^"]*\)".*/\1/p' || true)
  [ "$got" = "$VER" ] && break
  echo "  healthz=$got — жду ($i)"; sleep 5
done
[ "$got" = "$VER" ] || fail "healthz отдаёт '$got', ожидал '$VER'"
echo "  healthz: $got ✓"

sync_electron() { # dir, label
  local dir="$1" label="$2"
  [ -d "$dir" ] || { echo "  $label: каталог не найден, пропуск"; return; }
  (cd "$dir" && npm pkg set parserVersion="$V" >/dev/null && env -u DOTA_PARSER_DIST node scripts/install-parser.js >/dev/null)
  local sum_local sum_rel tmp
  tmp=$(mktemp)
  gh release download "$V" -R "$REPO_SLUG" -p parser-mac-arm64 -O "$tmp" --clobber
  sum_local=$(shasum -a 256 "$dir/bin/parser-mac-arm64" | cut -d' ' -f1)
  sum_rel=$(shasum -a 256 "$tmp" | cut -d' ' -f1)
  rm -f "$tmp"
  [ "$sum_local" = "$sum_rel" ] || fail "$label: bin/ не совпал с release-ассетом"
  echo "  $label: пин $V, чексумма ✓"
}

step "2/5 reso-coach-companion"
sync_electron "$COMPANION_DIR" "companion"

step "3/5 gamerjournal-replay-uploader (legacy — до вывода из эксплуатации)"
sync_electron "$UPLOADER_DIR" "uploader"

step "4/5 локальный бинарь GJ (DOTA_PARSER_BIN)"
(cd "$PARSER_REPO" && go build -o parser .)
got=$("$PARSER_REPO/parser" --version 2>/dev/null || echo "?")
[ "$got" = "$VER" ] || fail "локальный бинарь отдал '$got'"
echo "  $PARSER_REPO/parser: $got ✓"

step "5/5 осталось руками (авто-апдейт игрокам)"
cat <<EOF
  companion (Immortal-игроки получают через electron-updater):
    cd $COMPANION_DIR
    git add -A && git commit -m "chore: parser $V" && npm version patch
    npm run build && npm run release   # (команды релиза companion)
  uploader: рекомендован sunset — если ещё жив, тот же цикл.

Каналы синхронизированы на $V. Проверка одной строкой в любой момент:
  curl -s https://$FLY_APP.fly.dev/healthz; $PARSER_REPO/parser --version
EOF
