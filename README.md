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
  "startDateTime":   <unix sec>
  "players":         [ {...} x10 ]
  "teamfights":      [...]
  "roshanKills":     [...]
  ...
  "parserVersion":   "<semver>"
}
```

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
