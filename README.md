# dota-replay-parser

Standalone Go binary that parses Dota 2 `.dem` replays into a single JSON
document — match metadata, per-player stats, item purchases, ward placements,
roshan/buyback events, teamfight aggregates, position samples.

Built on [`dotabuff/manta`](https://github.com/dotabuff/manta).

Used by:
- [`gamerjournal-replay-uploader`](https://github.com/Resolut1onEDL/gamerjournal-replay-uploader) — Electron client that watches the local Dota 2 replay folder and uploads parsed JSON to GamerJournal.
- [`resoai-dota-coach`](https://github.com/Resolut1onEDL/resoai-dota-coach) — backfill / batch parsing for the coach knowledge base.

## Usage

```sh
parser path/to/match.dem > match.json
```

Stdout is the JSON output. Stderr carries progress logs. Exit code 0 = success.

## Output schema (top level)

```
{
  "id":              <int64>     // match_id
  "gameMode":        <int>
  "lobbyType":       <int>
  "didRadiantWin":   <bool>
  "durationSeconds": <int>
  "startDateTime":   <unix sec>  // first packet of demo (pick/strategy phase)
  "hornDateTime":    <unix sec>  // game-clock 0 (creeps spawn)
  "pauses":          [ { gameTime, wallStart, durationSec, pausedBy } ]
  "players":         [ {...} x10 ]
  "teamfights":      [...]
  "roshanKills":     [...]
  ...
  "damageSeconds":   [ { t, a, v, k, d } ]   // 4.9.0 wide export, see below
  "controlEvents":   [ { t, p, a, m, ab, stun, slow, on } ]
  "parserVersion":   "<semver>"
}
```

### Wide export (4.9.0, `wide.go`)

Raw timelines next to the aggregated counters, so a new metric over another
window or another pair of heroes is a recompute on the stored JSON instead of
a reparse of a `.dem` Valve deletes after ~14 days. Additive; no existing
field changes meaning.

- `damageSeconds` — damage to a real hero per game second: `a` attacker slot
  (its illusions included) or −1 lane creep / −2 neutral / −3 building /
  −4 other, `v` victim slot, `k` damage type (1 physical, 2 magical, 4 pure),
  `d` sum. Hits on illusions are left out. Not named `combatSeconds`: the web
  fight detector reserves that name for a hero→hero-only stream.
- `controlEvents` — modifiers the combat log gives a stun or a slow duration,
  applied to (`on`) and removed from (`!on`) a real hero.
- `players[].stats.vitals` — `{hp, hpMax, mana, manaMax}`, one value per game
  second from the horn (index = second).
- `players[].stats.itemGains` — `[{t, item, n}]`: an item arriving on the hero
  (a new item entity in the inventory, backpack or neutral slot, or more
  charges on one held). Healing Lotuses (`item_famango`) arrive only this way.

Size: ~3 MB JSON (~200 KB gzipped) for a 36-minute pub, about 4× 4.7.4.

Cast flags: `abilityCastEvents[].isStolen` comes from the ability entity's
`m_bStolen` (4.9.0) — Valve leaves the combat log's flag empty, so before
4.9.0 it was always false. `isUltimate` is still always false: no entity field
marks an ultimate, and the hero table it needs changes every patch — consumers
take it from Valve's hero datafeed (ResoAI-web `src/lib/ultimates-static.json`).

4.9.0 also carries what was developed in the web repo's vendored copy as
"4.8.0–4.8.1": every pregame stamp (purchases, wards, smokes, casts, damage)
is parked on an epoch and moved onto the game clock when
`m_flGameStartTime` arrives (it subsumes the skill-build-only 4.7.2 fix), and
hero positions are sampled before the horn too (smoke routes).

### Wall-clock alignment (e.g. voice transcripts → game events)

`startDateTime` points to the demo's first packet (pick/strategy). For aligning
external timestamps with in-game events, use `hornDateTime` (= unix sec at
game-clock 0). To map a wall-clock unix timestamp `T` to game seconds:

```
game_sec(T) = (T - hornDateTime) - Σ pause.durationSec for each pause where pause.wallStart <= T
```

All per-event times inside the output (`teamfights[].startTime`, `deaths[].time`,
`roshanKills[].time`, …) are game-clock seconds already (0 = horn, negative =
pregame), so this gives you the axis to compare against them.

Per-player fields include `steamAccountId`, `heroId`, `heroName`, `isRadiant`,
`isVictory`, `kills`/`deaths`/`assists`, `networth`, `goldPerMinute`,
`experiencePerMinute`, `numLastHits`, `numDenies`, `level`, `heroDamage`,
`towerDamage`, `heroHealing`, `lane`, `role`, `position`, item slots, and
nested `stats` (per-minute time series + event lists).

## Build

```sh
go build -o parser .
```

Pure-Go, no CGO. Cross-builds with `GOOS`/`GOARCH`:

```sh
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -o parser-mac-arm64 .
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o parser-linux-x64 .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o parser-win-x64.exe .
```

CI does this for every tag `v*` and attaches the three binaries to the GitHub
release. Consumers should pin a specific tag and download the artifact.

## Tests

`main_test.go` contains a regression test for player team assignment (parser
output `isRadiant`/`isVictory` per heroId vs. OpenDota ground truth). It
requires `.dem` files in `test-replays/` (not committed — see
[test-replays/README.md](test-replays/README.md)).

```sh
go test -timeout 600s
```

Tests skip gracefully if fixtures are missing.

## Versioning

Semver. Bump rules:
- **patch**: bug fixes that preserve JSON schema (e.g. fixing team detection — v3.1.1).
- **minor**: new fields added to JSON output (additive only).
- **major**: removed/renamed fields, breaking schema change.
