# Releasing the parser — one train, every channel

The parser has ONE source (this private repo; binaries only ever leave it as
release assets) and FOUR delivery channels. They do not update themselves —
history proof: prod once held 4.3.1 and 4.4.4 rows arriving the same day,
because each channel was pinned/deployed at a different moment.

| Channel | Consumer | How it gets the parser |
|---|---|---|
| GitHub release assets | everything below | CI builds on tag `v*` |
| Fly app `resoai-parse` | app.reso.coach (разборы, ODB-инжест) | built FROM SOURCE at `fly deploy` |
| reso-coach-companion | Immortal players (local parse → upload) | binaries baked at build, pinned by `package.json parserVersion`, shipped via electron-updater |
| gamerjournal-replay-uploader | legacy (sunset candidate) | same scheme as companion |
| local `./parser` binary | GamerJournal MCP tools (`DOTA_PARSER_BIN`) | `go build` by hand |

## The train

```bash
# 1. merge to main, then (manual by policy):
git tag vX.Y.Z && git push origin vX.Y.Z

# 2. after CI finishes, fan out + verify every channel:
scripts/release-sync.sh vX.Y.Z

# 3. ship companion so electron-updater reaches the players
#    (release-sync prints the exact commands)
```

## Rules that keep this working

- `parserVersion` const in main.go is the single version source; it must
  equal the tag. `parser --version` prints it; `/healthz` reports it.
- install-parser in companion/uploader downloads THE PIN from the release —
  the local `dist-release/` shortcut is opt-in via `DOTA_PARSER_DIST` only
  (the local-first default once shipped stale 4.3.1 to players).
- `dist-release/` loose files are dev leftovers, never a distribution source.
- Web keeps `compareParserVersions` guard: an older-parser upload never
  overwrites a newer parse of the same match.
- Checking sync state at any moment:
  `curl -s https://resoai-parse.fly.dev/healthz` + `./parser --version` +
  `grep parserVersion ../reso-coach-companion/package.json`.
