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
| ResoAI-web `parser/` | team dashboard + coaching students' dashboard (GitHub Actions build it from the web repo) | source copied from the tag by release-sync, committed in the web repo |

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
- `fly deploy` and the local `go build` ship the WORKING TREE, not git. release-sync
  refuses a `PARSER_REPO` that is not exactly the tag commit with a clean tree
  (`scripts/check-release-tree.sh`) — 2026-09-26 it shipped uncommitted 4.8.0
  work from the main checkout. Run it against a clean copy of the tag:
  `git worktree add --detach ../dota-replay-parser-vX.Y.Z vX.Y.Z`.
- Web keeps `compareParserVersions` guard: an older-parser upload never
  overwrites a newer parse of the same match.
- Checking sync state at any moment:
  `curl -s https://resoai-parse.fly.dev/healthz` + `./parser --version` +
  `grep parserVersion ../reso-coach-companion/package.json`.

## Before the tag: the audit

A release must not count anything worse than the last one. `cmd/audit` compares the
parser with Valve's numbers (exact) and OpenDota's parse of the same replays, field by
field; `audit/baseline.json` holds the lowest rate each field may have.

```bash
go build -o parser . && go build -o audit-bin ./cmd/audit
./audit-bin pick  -dir ~/dota-replays/audit -n 100      # or reuse the last set
./audit-bin fetch -dir ~/dota-replays/audit
./audit-bin run   -dir ~/dota-replays/audit -parser ./parser -baseline audit/baseline.json
```

`run` exits 1 and lists the fields when one matches worse than the baseline. A fix that
makes fields better is followed by `-write-baseline audit/baseline.json` and a commit of
the new baseline. The same check runs every Monday on ~20 fresh matches
(`.github/workflows/audit.yml`); `go test` runs it on the replays kept in `test-replays/`.
Known differences of the references (OpenDota samples minutes a second early, its starting
items are its first inventory sighting, Valve's XPM counts past level 30, …) are written in
`audit/compare.go`, next to the check they affect.

