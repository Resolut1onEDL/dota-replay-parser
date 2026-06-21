# Handoff: ship parser v4.3.0 to the GamerJournal desktop client

**Goal:** make future Team Jazz / personal games carry ward-vision data (observer
lifetime + dewards) so the AI-coach `get_player_warding` MCP tool answers in full
for our own players (Resolut1on pos4 etc.). Right now only **counts** of
obs/sentry are available for our games — duration & dewards require v4.3.0.

## What changed in the parser (v4.2.0 → v4.3.0)

Additive, backward-compatible. New fields in the JSON output:

- `players[].stats.wards[].durationSeconds` and `.endTime` — how long each ward
  lived (entity create → delete). **Absent (omitted) when the ward was still alive
  at game end** — by design; such wards are excluded from average-duration math.
- `players[].stats.visionStats.wardsDewarded` — count of enemy wards (obs+sentry)
  this player destroyed. Natural ward expiry is **not** counted (combat-log
  attacker == target = self-expiry; a real deward has a hero attacker).
- `parserVersion` is now `"4.3.0"`.

No fields were removed or renamed. Older consumers keep working.

## Why no GamerJournal/server change is needed

The upload route stores the whole blob and derives the version from it:

```
// src/app/api/games/upload/route.ts
parser_version: body.parser_version ?? parser.parserVersion ?? "unknown"
```

So once the client runs the v4.3.0 binary, `parser_output` already contains the
new ward fields **and** reports `parser_version = "4.3.0"` automatically. No DB
migration (the data rides inside the existing `games.parser_output` JSONB), and
the aggregator (`src/lib/teams/queries.ts → loadPlayerWarding`) already reads
`stats.wards[].durationSeconds` and `stats.visionStats.wardsDewarded`.

## Steps for the client team

1. **Release the parser at v4.3.0.** From `dota-replay-parser`:
   - commit the v4.3.0 changes (main.go + main_test.go) and push,
   - tag it (e.g. `v4.3.0`) — `.github/workflows/release.yml` cross-builds and
     attaches `parser-win-x64.exe`, `parser-linux-x64`, `parser-mac-arm64`.
   - (manual equivalent: `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o parser-win-x64.exe .`)
2. **Bump the bundled/pinned parser in the desktop client** to the v4.3.0
   `parser-win-x64.exe`. The client invokes it the same way (`parser <replay.dem>`,
   JSON to stdout) — no CLI/contract change.
3. **No upload-payload change required.** Keep POSTing `{ parser_output, match_id }`
   to `/api/games/upload`; the version and ward fields flow through.
4. **Verify** after one fresh game: in `games.parser_output`, confirm a support
   player has `stats.wards[i].durationSeconds` populated and
   `stats.visionStats.wardsDewarded` present, and `parser_version = '4.3.0'`.

## After the client ships

- Resolut1on plays fresh **pos4** Jazz games → they parse with v4.3.0 → ward
  lifetime/dewards land automatically → `get_player_warding(jazz, 4)` returns
  `avgObserverDurationSec` and `avgDewards` (not null), making the Q3
  Resolut1on-vs-Mira comparison fully live.
- Old replays cannot be backfilled (Valve CDN retains replays ~2 weeks; the
  parser-only path for ward data is inherently prospective).

## Verified

v4.3.0 validated on real pro replays: e.g. Aurora match 8841231956 — Mira
(Hoodwink) 22 obs / 32 sen, avg observer life 247.7s, 12 dewards; expiries vs
kills separated correctly. Unit tests `TestIsWardDeward`, `TestFinalizeWard` pass.
