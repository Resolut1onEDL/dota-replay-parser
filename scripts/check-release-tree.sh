#!/bin/bash
# check-release-tree.sh <parser-repo> vX.Y.Z — refuse to ship from a checkout
# that is not exactly the release tag. `fly deploy` and `go build` build the
# WORKING TREE, not git: on 2026-09-26 release-sync v4.7.2, run from the main
# checkout, shipped someone's uncommitted 4.8.0 work to resoai-parse.
set -euo pipefail
repo="$1" V="$2"

git -C "$repo" fetch -q origin --tags
want=$(git -C "$repo" rev-parse -q --verify "$V^{commit}") ||
  { echo "  $repo: тега $V нет"; exit 1; }
head=$(git -C "$repo" rev-parse HEAD)
dirty=$(git -C "$repo" status --porcelain)

if [ "$head" != "$want" ] || [ -n "$dirty" ]; then
  echo "  $repo — не чистый $V, из него собирать нельзя:"
  [ "$head" = "$want" ] || echo "    HEAD $(git -C "$repo" log --oneline -1 HEAD | cut -c1-70), а $V = ${want:0:7}"
  [ -z "$dirty" ] || { echo "    незакоммиченные/новые файлы:"; echo "$dirty" | sed 's/^/      /'; }
  echo "  Собери из чистой копии тега:"
  echo "    git -C $repo worktree add --detach $repo-$V $V"
  echo "    PARSER_REPO=$repo-$V scripts/release-sync.sh $V"
  exit 1
fi
echo "  $repo: ровно $V (${want:0:7}), чисто ✓"
