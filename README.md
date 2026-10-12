# dota-replay-parser

Standalone Go binary that parses Dota 2 `.dem` replays into a single JSON
document — match metadata, per-player stats, item purchases, ward placements,
roshan/buyback events, teamfight aggregates, position samples.

Built on [`dotabuff/manta`](https://github.com/dotabuff/manta) v1.4.7, used
through a patched copy in `third_party/manta` (see its `PATCHES.md`).

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
  "positionTrack":   [ { t, p } ]            // 4.13.0: 1 Hz hero positions, see below
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
  (since 4.10.0 its owner's: the player's illusions, summons and dominated
  creeps count as the player) or −1 lane creep / −2 neutral / −3 building /
  −4 other, `v` victim slot, `k` damage type (1 physical, 2 magical, 4 pure),
  `d` sum, `u: 1` (4.10.1) when the hits landed on the victim's Spirit Bear.
  Hits on illusions and Sunder's HP swap are left out. Not named `combatSeconds`: the web
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

### 4.10.0 — counted as Valve counts (the audit, `cmd/audit`)

Fields compared with Valve's numbers and OpenDota's parse of 99 fresh matches;
what changed meaning (the report: `audit/reports/2026-10-07-after.md`):

- `heroDamage`, `towerDamage` — Valve's own counters (`m_flHeroDamage`,
  `m_flTowerDamage`); `heroDamageReport`, `damageSeconds`, creep kills, Roshan
  and building kills and healing credit a summon's, dominated creep's or
  illusion's part to its owner; damage to illusions is no hero damage.
- `stunDurationDealt` — Valve's `m_fStuns`. `networth`, final items frozen at
  the game's end; final items, max health and positions ignore illusions.
- `killEvents`/`deathEvents` — no illusion deaths; a kill is the unit owner's;
  an Aegis or Reincarnation death has `reincarnated: true` (no kill, no death on
  the scoreboard); the killer is not in his own `assistEvents`.
- `stats.goldPerMinute` — gold earned so far (`m_iTotalEarnedGold`, OpenDota's
  `gold_t`); per-minute samples are the values at mm:00.
- `stats.itemPurchases` — starting items (the inventory when the hero is first
  seen, as OpenDota does — the strategy phase is not in the combat log); no
  `item_ward_dispenser` combine entries.
- `stats.skillBuild` — one entry per spent skill point plus talents (7.41
  talents cost no point), as Valve's ability_upgrades; ability names as the
  combat log writes them (`luna_lucent_beam`, was `luna_lucentbeam`).
- `stats.runes` — the game's own pickup announcements (wisdom runes included;
  a bottled rune at its use).
- `stats.totalDeadTimeSec` — from the dying state, up to the game's end.
- `pauses` and every entity-side time — Valve's pause counters; the entity
  clock no longer runs ahead of the combat log in paused games.

### 4.10.1 — Meepo and the Spirit Bear (the weekly audit, 2026-10-09)

- `killEvents`/`deathEvents`/`assistEvents` — when one Meepo dies the others
  die at the same tick, each logged with Meepo as the attacker. A player now
  dies once per tick (the death kept is the first not credited to himself, with
  the gold it lost); a death credited to the dying player is no kill and has no
  assists (9032885465: Meepo's 25 death and 16 kill events → 10 and 1, as the
  scoreboard). Lane kill/death/assist counts follow.
- `damageSeconds` — a hit on Lone Druid's Spirit Bear is a hit on its owner,
  as Valve's hero damage counts it (its death is still no kill), marked `u: 1`
  so damage to the hero itself can leave it out. `heroDamageReport` stays the
  heroes' (OpenDota's damage_targets lists the bear apart); `heroDamage` is
  Valve's counter, its combat-log fallback now counts the bear too.

### 4.10.2 — assists as Valve's scoreboard counts them

- `assistEvents` — one per rise of Valve's assist counter (`m_iAssists`), so
  their number is the scoreboard's `assists` (the audit: 52.4% → 100% of 990
  players). The combat log's assist list holds only the heroes that damaged the
  victim; the scoreboard also credits a disable without damage (Hex, Orchid,
  X Marks the Spot) and more (8664300772: Shadow Shaman 23 → 29). An event's
  `target` is the enemy who died at that tick; `-1` when no single enemy death
  fits (0.46% of events: two enemies died and neither lists the player, or only
  an ally died that tick).
- `deathEvents[].assist` — the players Valve credited for that death (an
  Aegis death lists none). `laneStats.assistsPreTen` follows the events.

### 4.11.0 — Tormentor kills

- `tormentorKills` (match) — every Tormentor killed, `[{time, team, killer}]`, from
  Valve's own announcement (CHAT_MESSAGE_MINIBOSS_KILL), as the aegis pickups: the
  killing team (its value: 2 radiant, 3 dire) and the hero behind the last hit
  (`killer`, `-1` when none; no omitempty — slot 0 is real). Absent when no
  Tormentor died. Until now the parse had no trace of a Tormentor at all: the
  coaching students' objectives tab took it from OpenDota, which parses few pubs.
  Checked on the audit set (99 matches of 2026-10-07): all 49 Tormentor kills
  OpenDota lists, each within 3 s, with the same team and the same killer; no
  audit field matches worse than the baseline.

### 4.12.0 — camp bans

- `spawnBlocks` (match) — every jungle camp that did not spawn at a minute mark
  with a player's unit in its box, the whole game: `[{time, campX, campY,
  campTier, player, by, dist}]`. Read from the camps themselves: the game creates
  a camp's next creeps ahead, parked at the spawner waiting to spawn
  (`m_bIsWaitingToSpawn`), and wakes them at the mark unless something stands in
  the box. A mark that did not wake them, with none of the camp's own creeps in
  the box (3.5 cells), is a ban; it goes to the player whose unit stood nearest
  the spawner — his hero, an illusion (Monkey King's soldiers too), a summon
  (Nature's Prophet's treants, a dominated creep), a courier or a ward — within
  6.5 cells and closer than any creep; `player: -1` when nobody's unit explains
  it or a lane creep / another camp's creep stood in the box. River frog camps
  (released one by one through the minute) are left out. `campTier` comes from
  the camp's species (the `UnitNames` table; replays before ~2026-10-10 lack it,
  then from the species heroes hit there). `campBlocks` (the laning block war,
  first 12 minutes) is unchanged.
  Checked on 9040460429 (BLAST SLAM VIII, Watson's Nature's Prophet: 19 of the
  enemy's ancient and medium camps banned with treants between 13 and 27 min)
  and on four replays' 5,690 camp-marks: with the camp's own creep in the box it
  woke 0.2 % of the time, with nobody within 6.5 cells 98.2 %, with a player's
  unit within 2.8 cells 29.5 % — a ban counts only a mark that did not wake.
  Audit set (99 matches of 2026-10-07): every other field byte-identical to
  4.11.0 in all 99, no audit field worse than the baseline; 23 bans a game
  (median), 21 % of them `player: -1` (a mark that did not wake with no
  player's unit within reach), camp tier unknown for 0.3 %. Parse time +~14 %.

### 4.13.0 — support exports: 1 Hz positions, stack outcomes, deward bounties

- `positionTrack` (match) — the ten heroes' positions once per game second, the
  whole game: `[{t, p}]`, `t` the game second (0 = horn, the combat-log axis of
  every event time), each second from 0 to `durationSeconds`; `p` =
  `[x0,y0,x1,y1,…,x9,y9]` in the cells of `positionSamples` (the real hero's
  `CBodyComponent.m_cellX/Y`, integers), `-1,-1` while the hero is dead (from the
  dying state on, as `totalDeadTimeSec`) or not seen yet. A row is the heroes as
  they stood at the first tick of its second. `positionSamples` (one row a
  minute) are unchanged; they are read up to a second later, keep a dead hero's
  last cell, and run on past the end.
- `players[].stats.stackEvents[]` — who cleared each stack: `clearedBy` (slot;
  no omitempty), `clearedAt` (his first kill of its creeps), `clearedCreeps`,
  `clearedGold` (the last three absent when `clearedBy` is `-1`). A stack's
  creeps are its camp's creeps (the camp from `m_hNeutralSpawner`, tracked since
  4.12.0) awake at the stack and alive then; a creep belongs to the earliest stack
  of its camp, so a camp stacked at 5:00 and 8:00 and cleared at 9:53 gives the
  5:00 stack the camp and its first stacked wave and the 8:00 stack the wave it
  added — no creep counts twice. Each creep's death (its `m_lifeState`, 1–3 ticks
  after the combat log) is joined to the combat log's neutral death, the killer's
  hero nearest the creep when two camps lose creeps in one tick; the killer is
  the owner of the unit that landed it (illusions, treants, skeletons count as
  their owner, as creep kills have since 4.10.0) and the gold is the neutral-kill
  gold Valve paid him in that tick (GOLD, reason 14), shared by the neutrals he
  killed in it. A creep a camp creep summoned (the dark troll warlord's
  skeletons) is not the camp's. The clearer killed the most of them (gold, the
  earlier first kill, the lower slot break a tie); `-1` when no player's unit
  killed any, or lane creeps of a pull killed more of them than he did.
  `clearedAt − time` can be long: a stacked camp nobody farms keeps its creeps.
- `stackEvents[]` camp (fix) — a stack goes to the nearest camp within reach
  (25 cells, as before) that shows a stack at that mark: a wave spawned with the
  minute while creeps it spawned earlier were alive, out of the box; one stack
  per camp and mark. Before, a counter rising by two at once — two camps side by
  side stacked in one pull, a medium and the ancients — put both stacks on the
  nearest camp, and some stacks landed on a nearer camp that did not spawn then.
- `players[].stats.dewardEvents` — `[{time, wardType, gold, goldTo, denied}]`,
  one per `visionStats.wardsDewarded` (the same combat-log ward deaths, so the
  counts are equal). `gold`: the bounty in Valve's announcement of that kill
  (`CHAT_MESSAGE_OBSERVER_WARD_KILLED` / `_SENTRY_WARD_KILLED`: playerid_1 the
  killer, playerid_2 the hero paid, value the gold; without one, the ward-kill
  GOLD, reason 20, of that tick). This patch pays for observers only (100 gold
  plus ~4 a minute of the game); a sentry is 0. `goldTo`: the slot paid — a
  quarter of the time not the killer: Valve seems to pay whoever revealed the
  ward (on the audit set the payee had a live sentry within 10 cells of the
  killer in 84 % of paid kills; dust and gems are not tracked), so a support's
  sentries earn him bounties for wards his team kills — sum the team's events
  whose `goldTo` is him. `-1` when nobody was paid (Valve named no one, or no
  announcement). `denied: true` — a ward of his own team (the combat log's
  attacker and target teams match): `wardsDewarded` counts these too; no bounty.
- Checked on sieg klas (1550818411, a position-4 Immortal), four games:
  Lion (won, 29:54), Rubick (lost, 45:27), Tusk (lost, 32:17), Skywrath Mage
  (won, 46:27). The track agrees with `positionSamples` at mm:00 to the cell in
  1,207 of 1,306 pairs, the rest a cell or two (a blink or a teleport inside the
  second); a hero is `-1` three seconds after every death but four, all
  buybacks. His 21 stacks: 17 cleared by his cores (2,879 gold; Lion's 6 of 6 by
  Sven, 0:09–4:53 after the stack), 2 by himself, 2 by the enemy offlaner
  (Timbersaw farmed two of Skywrath Mage's stacks at 18:12); a Rubick stack of
  the ancients at 10:00 sat until 24:09. Before the camp fix 9 of the 21 sat on
  another camp: 6 in the 6 double pulls (medium + ancients) counted on one camp,
  3 on a camp that did not spawn then.
  His 33 dewards: 20 sentries (0 gold), 13 observers (2,354 gold, 726 of it paid
  to a teammate); teammates' kills on his sentries paid him 481 more.
  Audit set (99 matches of 2026-10-07): no audit field worse than the baseline;
  every other field byte-identical to 4.12.0 but the stack camps — 547 of
  1,764 stacks moved: 257 that shared a camp and minute with another stack of
  the same player (253 repeats before, none now) and 290 that sat on a camp
  that did not spawn at that mark; the new camp is a median 5.7 cells from the
  stacker (the old 4.0) — and `spawnBlocks` ties (32 bans in 25 matches: two units at the same
  distance, which 4.12.0 itself picks either way run to run). Stacks: 45 %
  cleared by the stacker, 42 % by an ally, 11 % by an enemy, 2 % by no player;
  73 % within 2 minutes, 7 % after 5. Dewards: 2,724 events, equal to
  `wardsDewarded` for all 990 players; all 980 observer kills paid, no sentry;
  26 % paid to someone other than the killer, 35 denied, 5 with no payee.
  `positionTrack`: a row for every second in 99 of 99; +31 KB gzipped a game
  (median, max 101), the whole parse +24 % gzipped (141 KB median before).
  Parse time +~3 %.

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
