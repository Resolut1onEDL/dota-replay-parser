package main

// Regression test for team-per-player assignment.
// Ensures parser output isRadiant/isVictory matches OpenDota ground truth
// (the source of truth, not the previous parser output).
//
// Reproduces the bug from match 8788500456 where Radiant/Dire was swapped
// for several players because the parser hardcoded `IsRadiant: i < 5`.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type opendotaPlayer struct {
	HeroID    int   `json:"hero_id"`
	AccountID int64 `json:"account_id"`
	IsRadiant bool  `json:"isRadiant"`
}

type opendotaMatch struct {
	MatchID    int64            `json:"match_id"`
	RadiantWin bool             `json:"radiant_win"`
	Players    []opendotaPlayer `json:"players"`
}

// Unit test for buildTalents (v4.1.0 talent extraction).
// Talent slots arrive as map[slotIdx]AbilityEntityInfo collected from the
// hero entity's m_vecAbilities. Hero ability lists carry the 8 talents in
// tier order (two per tier), so with exactly 8 slots the tier is derivable
// (10/15/20/25); otherwise level must be omitted (0).
func TestBuildTalents(t *testing.T) {
	t.Run("full 8 slots derive tiers", func(t *testing.T) {
		slots := map[int]AbilityEntityInfo{
			10: {Name: "special_bonus_unique_axe_8", Level: 1}, // tier 10 left
			11: {Name: "special_bonus_movement_speed_20"},      // tier 10 right, not chosen
			12: {Name: "special_bonus_strength_12"},            // tier 15 left, not chosen
			13: {Name: "special_bonus_unique_axe_4", Level: 1}, // tier 15 right
			14: {Name: "special_bonus_unique_axe_5", Level: 1}, // tier 20 left
			15: {Name: "special_bonus_hp_500"},                 // tier 20 right, not chosen
			16: {Name: "special_bonus_unique_axe_2", Level: 1}, // tier 25 left
			17: {Name: "special_bonus_unique_axe_3"},           // tier 25 right, not chosen
		}
		got := buildTalents(slots)
		want := []struct {
			level int
			name  string
		}{
			{10, "special_bonus_unique_axe_8"},
			{15, "special_bonus_unique_axe_4"},
			{20, "special_bonus_unique_axe_5"},
			{25, "special_bonus_unique_axe_2"},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d talents, want %d: %+v", len(got), len(want), got)
		}
		for i, w := range want {
			if got[i].Level != w.level || got[i].Name != w.name {
				t.Errorf("talent[%d]: got {%d %s}, want {%d %s}",
					i, got[i].Level, got[i].Name, w.level, w.name)
			}
		}
	})

	t.Run("excluded generic special_bonus abilities", func(t *testing.T) {
		slots := map[int]AbilityEntityInfo{
			5:  {Name: "special_bonus_attributes", Level: 7},
			6:  {Name: "special_bonus_base", Level: 1},
			10: {Name: "special_bonus_unique_axe_8", Level: 1},
		}
		got := buildTalents(slots)
		if len(got) != 1 || got[0].Name != "special_bonus_unique_axe_8" {
			t.Fatalf("expected only the real talent, got %+v", got)
		}
	})

	t.Run("non-8 slot count omits tier level", func(t *testing.T) {
		slots := map[int]AbilityEntityInfo{
			12: {Name: "special_bonus_unique_a", Level: 1},
			14: {Name: "special_bonus_unique_b", Level: 1},
		}
		got := buildTalents(slots)
		if len(got) != 2 {
			t.Fatalf("got %d talents, want 2: %+v", len(got), got)
		}
		for _, tt := range got {
			if tt.Level != 0 {
				t.Errorf("level must be omitted (0) when tier not derivable, got %d", tt.Level)
			}
		}
		// Slot order preserved
		if got[0].Name != "special_bonus_unique_a" || got[1].Name != "special_bonus_unique_b" {
			t.Errorf("slot order not preserved: %+v", got)
		}
	})

	t.Run("empty input yields empty non-nil slice", func(t *testing.T) {
		got := buildTalents(map[int]AbilityEntityInfo{})
		if got == nil {
			t.Fatal("talents must be non-nil so JSON emits [] not null")
		}
		if len(got) != 0 {
			t.Fatalf("expected empty, got %+v", got)
		}
	})
}

// Regression tests for hero resolution (v4.1.1).
// Match 8824123966 ground truth (Stratz): Faceless Void (41) and Largo (155)
// came out as heroId=0 / "Hero_0" because:
//   - entity class suffixes like "FacelessVoid" had no no-separator alias in
//     the npc-name map (only "faceless_void"), so the class-name fallback failed;
//   - Largo (155) and Kez (152) were missing from the compiled tables entirely;
//   - m_nSelectedHeroID is exposed as uint32 in newer replays (GetInt32 read
//     failed) and carries the hero ID doubled — same encoding artifact as
//     m_iPlayerID (see replayPlayerToIndex).
func TestHeroNameStringToID(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		// combat-log npc suffixes (worked before the fix — regression guard)
		{"faceless_void", 41},
		{"axe", 2},
		{"void_spirit", 126},
		// entity class suffixes (CDOTA_Unit_Hero_*) — failed before the fix
		{"FacelessVoid", 41},
		{"Void_Spirit", 126},
		{"Sand_King", 16},
		// new heroes — missing from tables before the fix
		{"largo", 155},
		{"Largo", 155},
		{"kez", 145}, // real PlayerResource id (152 does not exist; was a map typo)
		{"ringmaster", 131},
		// unknown stays 0, never invented
		{"totally_unknown_hero", 0},
	}
	for _, c := range cases {
		if got := heroNameStringToID(c.in); got != c.want {
			t.Errorf("heroNameStringToID(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestDecodeSelectedHeroID(t *testing.T) {
	cases := []struct {
		raw    interface{}
		want   int
		wantOK bool
	}{
		// legacy replays: plain int32
		{int32(41), 41, true},
		{int32(0), 0, true},
		// newer replays: uint32 with the ID doubled (match 8824123966:
		// slot0=82→41 Faceless Void, slot7=310→155 Largo, slot6=26→13 Puck)
		{uint32(82), 41, true},
		{uint32(310), 155, true},
		{uint32(26), 13, true},
		{uint32(0), 0, true},
		// property absent (manta Get returns nil)
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := decodeSelectedHeroID(c.raw)
		if got != c.want || ok != c.wantOK {
			t.Errorf("decodeSelectedHeroID(%v %T) = (%d,%v), want (%d,%v)",
				c.raw, c.raw, got, ok, c.want, c.wantOK)
		}
	}
}

func TestGetHeroNameNewHeroes(t *testing.T) {
	cases := []struct {
		id   int
		want string
	}{
		{41, "Faceless Void"},
		{145, "Kez"}, // real id (152 never existed — old map typo)
		{131, "Ringmaster"},
		{155, "Largo"},
		{9999, "Hero_9999"}, // unknown id keeps explicit fallback
	}
	for _, c := range cases {
		if got := getHeroName(c.id); got != c.want {
			t.Errorf("getHeroName(%d) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestTeamAssignmentMatchesOpenDota(t *testing.T) {
	cases := []string{"8582691771", "8591372106", "8591453147"}

	bin := filepath.Join(t.TempDir(), "parser")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	for _, mid := range cases {
		t.Run(mid, func(t *testing.T) {
			demPath := filepath.Join("test-replays", mid+".dem")
			odPath := filepath.Join("test-replays", mid+"_opendota.json")
			if _, err := os.Stat(demPath); err != nil {
				t.Skipf("no replay: %v", err)
			}
			if _, err := os.Stat(odPath); err != nil {
				t.Skipf("no opendota json: %v", err)
			}

			out, err := exec.Command(bin, demPath).Output()
			if err != nil {
				t.Fatalf("parser run: %v", err)
			}
			var got Match
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("parser stdout not JSON: %v", err)
			}

			raw, err := os.ReadFile(odPath)
			if err != nil {
				t.Fatal(err)
			}
			var od opendotaMatch
			if err := json.Unmarshal(raw, &od); err != nil {
				t.Fatal(err)
			}

			if got.DidRadiantWin != od.RadiantWin {
				t.Errorf("didRadiantWin: got=%v want=%v", got.DidRadiantWin, od.RadiantWin)
			}

			truthByHero := make(map[int]bool, len(od.Players))
			for _, p := range od.Players {
				truthByHero[p.HeroID] = p.IsRadiant
			}

			for _, p := range got.Players {
				want, ok := truthByHero[p.HeroID]
				if !ok {
					t.Errorf("hero %d (%s) missing from opendota ground truth", p.HeroID, p.HeroName)
					continue
				}
				if p.IsRadiant != want {
					t.Errorf("hero %d (%s) isRadiant: got=%v want=%v",
						p.HeroID, p.HeroName, p.IsRadiant, want)
				}
				wantVictory := want == od.RadiantWin
				if p.IsVictory != wantVictory {
					t.Errorf("hero %d (%s) isVictory: got=%v want=%v",
						p.HeroID, p.HeroName, p.IsVictory, wantVictory)
				}
			}
		})
	}
}

// v4.3.0: deward attribution. Combat-log помечает истёкший вард как
// attacker == target; снос врагом — attacker = герой. isWardDeward отделяет одно
// от другого, чтобы естественное истечение не засчитывалось как deward.
func TestIsWardDeward(t *testing.T) {
	cases := []struct {
		name, target, attacker string
		want                   bool
	}{
		{"sentry killed by hero", "npc_dota_sentry_wards", "npc_dota_hero_hoodwink", true},
		{"observer killed by hero", "npc_dota_observer_wards", "npc_dota_hero_bane", true},
		{"sentry expired", "npc_dota_sentry_wards", "npc_dota_sentry_wards", false},
		{"observer expired", "npc_dota_observer_wards", "npc_dota_observer_wards", false},
		{"not a ward", "npc_dota_hero_axe", "npc_dota_hero_lina", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isWardDeward(c.target, c.attacker); got != c.want {
				t.Errorf("isWardDeward(%q,%q) = %v, want %v", c.target, c.attacker, got, c.want)
			}
		})
	}
}

// v4.3.0: ward lifetime. finalizeWard на удалении сущности проставляет
// EndTime/Duration в WardEvent игрока; варды без удаления (живы до конца) Duration
// не получают и не портят среднюю.
func TestFinalizeWard(t *testing.T) {
	mkState := func() *ParserState {
		s := &ParserState{ActiveWards: make(map[int32]*activeWard)}
		s.Players[3] = &PlayerState{Wards: []WardEvent{{Time: 100, Type: 0}}}
		s.ActiveWards[42] = &activeWard{playerIdx: 3, sliceIdx: 0, start: 100}
		return s
	}

	t.Run("sets duration on deletion", func(t *testing.T) {
		s := mkState()
		finalizeWard(s, 42, 360) // прожил 260с
		w := s.Players[3].Wards[0]
		if w.EndTime != 360 || w.Duration != 260 {
			t.Errorf("EndTime=%v Duration=%v, want 360/260", w.EndTime, w.Duration)
		}
		if _, ok := s.ActiveWards[42]; ok {
			t.Error("entry not removed from ActiveWards")
		}
	})

	t.Run("unknown entity is a no-op", func(t *testing.T) {
		s := mkState()
		finalizeWard(s, 999, 360)
		if s.Players[3].Wards[0].Duration != 0 {
			t.Error("untracked deletion mutated a ward")
		}
	})

	t.Run("negative duration clamped to 0", func(t *testing.T) {
		s := mkState()
		finalizeWard(s, 42, 50) // now < start
		if s.Players[3].Wards[0].Duration != 0 {
			t.Errorf("Duration=%v, want 0", s.Players[3].Wards[0].Duration)
		}
	})
}

// assignPositions must ALWAYS emit a clean 1..5 permutation per team — the
// bucket-per-lane version it replaced could not: a safe-lane trilane produced
// 1+5+5 and no pos-4 (live case: match 8919464063, Dire [5,1,3,5,2] — the
// roaming four and the hard five both labelled 5).
func TestAssignPositionsTrilanePermutation(t *testing.T) {
	mkPlayer := func(radiant bool, nwAtAnchor int) *PlayerState {
		ps := &PlayerState{IsRadiant: radiant, NetWorth: nwAtAnchor * 2}
		// 30 minutes of snapshots, linear growth to nwAtAnchor*2 at the end.
		for m := 1; m <= 30; m++ {
			ps.MinuteSnapshots = append(ps.MinuteSnapshots, MinuteSnapshot{NW: nwAtAnchor * 2 * m / 30})
		}
		return ps
	}
	state := &ParserState{}
	// Dire mirrors Даня's game: trilane on safe (rich Lifestealer, roaming
	// Pudge, poor Lion), SF mid, Rubick alone on off.
	state.Players[0] = mkPlayer(false, 12000) // Lifestealer, safe
	state.Players[1] = mkPlayer(false, 5000)  // Pudge, safe (roams)
	state.Players[2] = mkPlayer(false, 3500)  // Lion, safe
	state.Players[3] = mkPlayer(false, 11000) // SF, mid
	state.Players[4] = mkPlayer(false, 4800)  // Rubick, off solo
	// Radiant: an ordinary 2-1-2.
	state.Players[5] = mkPlayer(true, 13000) // carry, safe
	state.Players[6] = mkPlayer(true, 4000)  // hard support, safe
	state.Players[7] = mkPlayer(true, 11500) // mid
	state.Players[8] = mkPlayer(true, 9000)  // offlaner
	state.Players[9] = mkPlayer(true, 5500)  // soft support, off

	lanes := map[int]string{
		0: "safe", 1: "safe", 2: "safe", 3: "mid", 4: "off",
		5: "safe", 6: "safe", 7: "mid", 8: "off", 9: "off",
	}
	pos := assignPositions(state, lanes)

	want := map[int]int{
		0: 1, // richest of the trilane → carry
		3: 2,
		4: 3, // alone on off → offlaner (Даня: «меня провозгласили тройкой»)
		1: 4, // richer leftover → the roaming four
		2: 5,
		5: 1, 6: 5, 7: 2, 8: 3, 9: 4,
	}
	for idx, p := range want {
		if pos[idx] != p {
			t.Errorf("player %d: got pos %d, want %d", idx, pos[idx], p)
		}
	}
	// The invariant itself, both teams.
	for _, team := range [][]int{{0, 1, 2, 3, 4}, {5, 6, 7, 8, 9}} {
		seen := map[int]bool{}
		for _, idx := range team {
			seen[pos[idx]] = true
		}
		for s := 1; s <= 5; s++ {
			if !seen[s] {
				t.Errorf("team %v: slot %d missing — not a permutation: %v", team, s, pos)
			}
		}
	}
}

// v4.6.0: stun combos. A clean chain (second disable lands as the first
// ends) and a simultaneous overlap must BOTH count as combos but differ in
// stunOverlapWastedSec — that difference is the whole point of the metric.
func TestDetectStunCombos(t *testing.T) {
	mkState := func() *ParserState {
		s := &ParserState{}
		for i := 0; i < 10; i++ {
			s.Players[i] = &PlayerState{IsRadiant: i < 5}
		}
		return s
	}

	t.Run("clean chain vs overlap are distinguished", func(t *testing.T) {
		s := mkState()
		// Clean chain on target 5: A(0) 100.0+1.2s, B(1) at 101.2 (gap 0).
		// Overlapping duo on target 6: A(0) 200.0+1.6s, B(1) at 200.2+1.7s.
		s.StunApps = []stunApp{
			{T: 100.0, Attacker: 0, Target: 5, Dur: 1.2, Ability: "sven_storm_bolt", AttackerShort: "sven", TargetShort: "medusa"},
			{T: 101.2, Attacker: 1, Target: 5, Dur: 1.0, Ability: "lion_impale", AttackerShort: "lion", TargetShort: "medusa"},
			{T: 200.0, Attacker: 0, Target: 6, Dur: 1.6, Ability: "sven_storm_bolt", AttackerShort: "sven", TargetShort: "lina"},
			{T: 200.2, Attacker: 1, Target: 6, Dur: 1.7, Ability: "lion_impale", AttackerShort: "lion", TargetShort: "lina"},
		}
		counts, wasted, events := detectStunCombos(s)
		if counts[0] != 2 || counts[1] != 2 {
			t.Fatalf("combo counts: got %d/%d, want 2/2", counts[0], counts[1])
		}
		// Clean chain wastes 0; the overlap burns 201.6-200.2 = 1.4s.
		if wasted[0] != 1.4 || wasted[1] != 1.4 {
			t.Errorf("wasted: got %.2f/%.2f, want 1.40/1.40", wasted[0], wasted[1])
		}
		if len(events[0]) != 2 {
			t.Fatalf("events[0]: got %d, want 2", len(events[0]))
		}
		ev := events[0][0]
		if ev.Time != 100.0 || ev.TargetHero != "medusa" {
			t.Errorf("first combo event: got %+v", ev)
		}
		if len(ev.Abilities) != 2 || ev.Abilities[0] != "sven_storm_bolt" || ev.Abilities[1] != "lion_impale" {
			t.Errorf("abilities: got %v", ev.Abilities)
		}
		if len(ev.Partners) != 1 || ev.Partners[0] != "lion" {
			t.Errorf("partners for sven: got %v", ev.Partners)
		}
	})

	t.Run("same ally re-stun is not a combo", func(t *testing.T) {
		s := mkState()
		// Slardar crush → bash: one attacker, chained control, no combo.
		s.StunApps = []stunApp{
			{T: 100.0, Attacker: 0, Target: 5, Dur: 0.8, Ability: "slardar_slithereen_crush", AttackerShort: "slardar", TargetShort: "medusa"},
			{T: 100.7, Attacker: 0, Target: 5, Dur: 1.0, Ability: "slardar_bash", AttackerShort: "slardar", TargetShort: "medusa"},
		}
		counts, wasted, events := detectStunCombos(s)
		if counts[0] != 0 || wasted[0] != 0 || len(events[0]) != 0 {
			t.Errorf("same-ally chain must not count: counts=%d wasted=%.2f events=%d",
				counts[0], wasted[0], len(events[0]))
		}
	})

	t.Run("gap over grace breaks the chain", func(t *testing.T) {
		s := mkState()
		// A ends at 101.2; B lands at 101.8 — 0.6s of free target, no chain.
		s.StunApps = []stunApp{
			{T: 100.0, Attacker: 0, Target: 5, Dur: 1.2, Ability: "sven_storm_bolt", AttackerShort: "sven", TargetShort: "medusa"},
			{T: 101.8, Attacker: 1, Target: 5, Dur: 1.0, Ability: "lion_impale", AttackerShort: "lion", TargetShort: "medusa"},
		}
		counts, _, _ := detectStunCombos(s)
		if counts[0] != 0 || counts[1] != 0 {
			t.Errorf("broken chain must not count: got %d/%d", counts[0], counts[1])
		}
	})
}

// v4.6.0: pulls. The dire support walks the wave into a camp (creep<->neutral
// deaths + neutral deletions at the camp) — attributed to the nearest hero
// over the pre-cluster window. A stack has no creep<->neutral deaths at all
// and must yield zero pulls even with the hero standing at the camp.
func TestDetectPulls(t *testing.T) {
	mkState := func() *ParserState {
		s := &ParserState{}
		for i := 0; i < 10; i++ {
			s.Players[i] = &PlayerState{IsRadiant: i < 5}
		}
		s.CampSpawners = [][2]float64{{164, 98}, {136, 148}}
		return s
	}
	campSamples := func(from, to float64, x, y float64) []posSample {
		var out []posSample
		for tt := from; tt <= to; tt++ {
			out = append(out, posSample{T: tt, X: x, Y: y})
		}
		return out
	}

	t.Run("support pull attributed, carry in lane is not", func(t *testing.T) {
		s := mkState()
		s.PullDeaths = []pullDeathRec{
			{T: 265, Radiant: false},                  // neutral died to dire creeps
			{T: 270, Radiant: false, CreepDied: true}, // dire creep died to neutrals
			{T: 273, Radiant: false},
		}
		s.NeutDeletions = []neutDeletion{{T: 269, X: 164, Y: 97}, {T: 274, X: 165, Y: 98}}
		s.PosHistory[5] = campSamples(250, 270, 163, 97)  // support at the camp
		s.PosHistory[6] = campSamples(250, 270, 176, 88)  // carry in lane, ~16 cells off
		pulls := detectPulls(s)
		if len(pulls[5]) != 1 {
			t.Fatalf("support pulls: got %d, want 1 (%+v)", len(pulls[5]), pulls[5])
		}
		ev := pulls[5][0]
		if ev.Time != 265 || ev.CampX != 164 || ev.CampY != 98 || ev.CreepsDied != 1 {
			t.Errorf("pull event: got %+v, want t=265 camp=(164,98) creepsDied=1", ev)
		}
		if len(pulls[6]) != 0 {
			t.Errorf("carry must not get the pull: %+v", pulls[6])
		}
	})

	t.Run("stack is not a pull", func(t *testing.T) {
		s := mkState()
		// Hero farms/stacks the camp: neutral deletions happen, hero is right
		// there — but no lane creep ever fought a neutral.
		s.NeutDeletions = []neutDeletion{{T: 269, X: 164, Y: 97}}
		s.PosHistory[5] = campSamples(250, 270, 163, 97)
		pulls := detectPulls(s)
		for i := 0; i < 10; i++ {
			if len(pulls[i]) != 0 {
				t.Fatalf("stack produced a pull for player %d: %+v", i, pulls[i])
			}
		}
	})

	t.Run("wave meeting neutrals without a puller is skipped", func(t *testing.T) {
		s := mkState()
		s.PullDeaths = []pullDeathRec{{T: 265, Radiant: false, CreepDied: true}}
		s.NeutDeletions = []neutDeletion{{T: 267, X: 164, Y: 97}}
		// Everyone far away (>20 cells from the camp).
		s.PosHistory[5] = campSamples(250, 270, 120, 140)
		pulls := detectPulls(s)
		for i := 0; i < 10; i++ {
			if len(pulls[i]) != 0 {
				t.Fatalf("unattended wave counted as pull for player %d", i)
			}
		}
	})
}

// v4.6.0 pull v2: attribution follows the FIRST poke into the neutrals —
// the support who aggroed the camp gets the pull even when the carry
// farms the pulled creeps standing closer at fight time (live case: Mirana
// vs SF, bot lane 8922693443).
func TestDetectPullsFirstPoke(t *testing.T) {
	mkState := func() *ParserState {
		s := &ParserState{CampWaveRuns: make(map[int]*campRuns)}
		for i := 0; i < 10; i++ {
			s.Players[i] = &PlayerState{IsRadiant: i < 5}
		}
		s.CampSpawners = [][2]float64{{158, 88}}
		return s
	}

	t.Run("earliest poke wins over nearest body", func(t *testing.T) {
		s := mkState()
		// Radiant wave parked in the camp 155-170s.
		s.CampWaveRuns[0] = &campRuns{Rad: []waveRun{{T0: 155, T1: 170}}}
		s.NeutDeletions = []neutDeletion{{T: 168, X: 158, Y: 87}}
		// Support (4) poked at 148, carry (0) farmed the camp from 158.
		s.HeroNeutHits = []neutHit{
			{T: 148, Player: 4, X: 160, Y: 84, Species: "kobold_tunneler"},
			{T: 158, Player: 0, X: 158, Y: 86, Species: "kobold_tunneler"},
		}
		s.PullDeaths = []pullDeathRec{{T: 162, Radiant: true}}
		pulls := detectPulls(s)
		if len(pulls[4]) != 1 || len(pulls[0]) != 0 {
			t.Fatalf("first poker must win: support=%v carry=%v", pulls[4], pulls[0])
		}
		if pulls[4][0].Time != 148 || pulls[4][0].OntoEnemyWave {
			t.Errorf("event: %+v, want time=148 ontoEnemyWave=false", pulls[4][0])
		}
	})

	t.Run("pull without any creep deaths is still seen", func(t *testing.T) {
		s := mkState()
		// Small camp melted by wave+hero: zero creep<->neutral deaths, only
		// wave presence + a deletion at the camp (live case: Mirana 4:30).
		s.CampWaveRuns[0] = &campRuns{Rad: []waveRun{{T0: 272, T1: 284}}}
		s.NeutDeletions = []neutDeletion{{T: 280, X: 158, Y: 88}}
		s.HeroNeutHits = []neutHit{{T: 268, Player: 4, X: 159, Y: 85, Species: "gnoll_assassin"}}
		pulls := detectPulls(s)
		if len(pulls[4]) != 1 {
			t.Fatalf("deathless pull missed: %+v", pulls)
		}
		if pulls[4][0].CreepsDied != 0 {
			t.Errorf("creepsDied: got %d, want 0", pulls[4][0].CreepsDied)
		}
	})

	t.Run("stack-pull onto the enemy wave flags direction", func(t *testing.T) {
		s := mkState()
		// RADIANT wave engaged, but the poker is DIRE (Io 4:23 case).
		s.CampWaveRuns[0] = &campRuns{Rad: []waveRun{{T0: 265, T1: 280}}}
		s.NeutDeletions = []neutDeletion{{T: 275, X: 158, Y: 88}}
		s.HeroNeutHits = []neutHit{{T: 263, Player: 7, X: 156, Y: 90, Species: "centaur_khan"}}
		s.PullDeaths = []pullDeathRec{{T: 270, Radiant: true, CreepDied: true}}
		pulls := detectPulls(s)
		if len(pulls[7]) != 1 {
			t.Fatalf("offensive pull missed: %+v", pulls)
		}
		ev := pulls[7][0]
		if !ev.OntoEnemyWave || ev.CreepsDied != 1 {
			t.Errorf("event: %+v, want ontoEnemyWave=true creepsDied=1", ev)
		}
	})

	t.Run("idle wave near an empty camp is not a pull", func(t *testing.T) {
		s := mkState()
		// Wave parked near the camp but no neutral evidence at all.
		s.CampWaveRuns[0] = &campRuns{Rad: []waveRun{{T0: 300, T1: 330}}}
		s.HeroNeutHits = []neutHit{{T: 298, Player: 4, X: 159, Y: 85, Species: "gnoll_assassin"}}
		pulls := detectPulls(s)
		for i := 0; i < 10; i++ {
			if len(pulls[i]) != 0 {
				t.Fatalf("empty-camp idle counted as pull: %+v", pulls[i])
			}
		}
	})
}

// v4.6.0: stack events from the authoritative m_iCampsStacked counter —
// located at the camp the stacker was running out of, tiered empirically.
func TestDetectStacks(t *testing.T) {
	s := &ParserState{}
	for i := 0; i < 10; i++ {
		s.Players[i] = &PlayerState{IsRadiant: i < 5}
	}
	s.CampSpawners = [][2]float64{{90, 158}, {158, 88}}
	// Stacker at the big camp during x:53-x:00, credit fires at 120.2.
	for tt := 110.0; tt <= 121; tt++ {
		s.PosHistory[8] = append(s.PosHistory[8], posSample{T: tt, X: 92, Y: 156})
	}
	s.StackIncrs = []stackIncr{{T: 120.2, Player: 8}}
	// Tier votes: ursa warrior (large leader) poked at that camp.
	s.HeroNeutHits = []neutHit{{T: 115, Player: 8, X: 91, Y: 157, Species: "polar_furbolg_ursa_warrior"}}
	events := detectStacks(s, campTiers(s))
	if len(events[8]) != 1 {
		t.Fatalf("stack events: %+v", events)
	}
	ev := events[8][0]
	if ev.CampX != 90 || ev.CampY != 158 || ev.CampTier != "large" {
		t.Errorf("event: %+v, want camp (90,158) tier large", ev)
	}
	// Increment with nobody near any camp → count-only, no event.
	s2 := &ParserState{CampSpawners: [][2]float64{{90, 158}}}
	for i := 0; i < 10; i++ {
		s2.Players[i] = &PlayerState{}
	}
	s2.StackIncrs = []stackIncr{{T: 240, Player: 3}}
	ev2 := detectStacks(s2, campTiers(s2))
	if len(ev2[3]) != 0 {
		t.Errorf("unlocated increment must not produce an event: %+v", ev2[3])
	}
}

// v4.6.0: camp-block war. A sentry in the spawn box blocks from placement
// to ward death; a hero in an EMPTY un-warded box at a minute tick is a
// body block; an occupied camp can't be blocked.
func TestDetectCampBlocks(t *testing.T) {
	mk := func() *ParserState {
		s := &ParserState{}
		for i := 0; i < 10; i++ {
			s.Players[i] = &PlayerState{IsRadiant: i < 5}
		}
		s.CampSpawners = [][2]float64{{96, 164}}
		s.CampSeenTimes = make([][]float64, 1)
		s.CampDelTimes = make([][]float64, 1)
		return s
	}

	t.Run("sentry in box blocks until dewarded", func(t *testing.T) {
		s := mk()
		s.Players[9].Wards = []WardEvent{{
			Time: 18, Type: 1, PlayerID: 9, PositionX: 100, PositionY: 166, EndTime: 305,
		}}
		blocks := detectCampBlocks(s)
		if len(blocks) != 1 {
			t.Fatalf("blocks: %+v", blocks)
		}
		b := blocks[0]
		if b.Method != "ward" || b.WardType != "sentry" || b.Player != 9 || b.From != 18 || b.To != 305 {
			t.Errorf("block: %+v", b)
		}
	})

	t.Run("ward far from any camp does not block", func(t *testing.T) {
		s := mk()
		s.Players[2].Wards = []WardEvent{{Time: 30, Type: 0, PositionX: 130, PositionY: 130}}
		if blocks := detectCampBlocks(s); len(blocks) != 0 {
			t.Fatalf("river ward counted as block: %+v", blocks)
		}
	})

	t.Run("body block only on an empty camp", func(t *testing.T) {
		s := mk()
		// Camp occupied through minute 1 (seen at 40, never deleted) —
		// hero standing there at 1:00 is stacking/farming, not blocking.
		s.CampSeenTimes[0] = []float64{40}
		for tt := 55.0; tt <= 125; tt++ {
			s.PosHistory[3] = append(s.PosHistory[3], posSample{T: tt, X: 96, Y: 163})
		}
		blocks := detectCampBlocks(s)
		if len(blocks) != 0 {
			t.Fatalf("occupied camp body-blocked: %+v", blocks)
		}
		// Camp emptied at 70 → the 2:00 spawn is body-blocked.
		s.CampDelTimes[0] = []float64{70}
		blocks = detectCampBlocks(s)
		if len(blocks) != 1 || blocks[0].Method != "body" || blocks[0].Player != 3 || blocks[0].From != 120 {
			t.Fatalf("body block missing: %+v", blocks)
		}
	})
}

// v4.6.0: measured dead time replaces guessing — a span is matched to its
// death event, aegis-style instant revives keep their true short span, and
// spans without a death event (illusion noise) still count in the total.
func TestApplyDeadSpans(t *testing.T) {
	events := []DeathEvent{{Time: 1000, TimeDead: 100}, {Time: 1009, TimeDead: 100}}
	spans := []deadSpan{
		{T0: 1000.5, T1: 1005.5}, // aegis death: 5s real, table said 100
		{T0: 1009.4, T1: 1062.4}, // real death: 53s
	}
	total := applyDeadSpans(spans, events)
	if events[0].DeadDurationSec != 5 || events[1].DeadDurationSec != 53 {
		t.Errorf("per-death spans: %+v", events)
	}
	if total != 58 {
		t.Errorf("total: got %v, want 58", total)
	}
	// Span with no event still counts toward the total.
	total2 := applyDeadSpans([]deadSpan{{T0: 50, T1: 60}}, nil)
	if total2 != 10 {
		t.Errorf("unmatched span total: got %v, want 10", total2)
	}
}

// v4.6.0: highground episodes. Two zone touches within 10s merge into one
// episode; a death inside truncates it (corpse freezes in zone until the
// respawn jump); outcome covers [entry, exit+10].
func TestDetectHgEntries(t *testing.T) {
	mkState := func() *ParserState {
		s := &ParserState{}
		for i := 0; i < 10; i++ {
			s.Players[i] = &PlayerState{IsRadiant: i < 5}
		}
		return s
	}
	// Default zones: radiant base edge x+y<=178. In-zone (100,76)=176;
	// out-of-zone lane point (110,80)=190. Player 5 is Dire → enemy base is
	// the Radiant plateau.
	inX, inY := 100.0, 76.0
	outX, outY := 110.0, 80.0

	t.Run("two touches within 8s merge into one episode", func(t *testing.T) {
		s := mkState()
		s.PosHistory[5] = []posSample{
			{T: 1000, X: outX, Y: outY},
			{T: 1001, X: inX, Y: inY},
			{T: 1004, X: inX, Y: inY},
			{T: 1005, X: outX, Y: outY}, // brief backstep
			{T: 1012, X: inX, Y: inY},   // re-entry 8s after last in-zone sample
			{T: 1015, X: inX, Y: inY},
			{T: 1016, X: outX, Y: outY},
		}
		entries := detectHgEntries(s)
		if len(entries[5]) != 1 {
			t.Fatalf("episodes: got %d, want 1 merged (%+v)", len(entries[5]), entries[5])
		}
		e := entries[5][0]
		if e.Time != 1001 || e.DurationSec != 14 {
			t.Errorf("merged episode: got t=%v dur=%v, want t=1001 dur=14", e.Time, e.DurationSec)
		}
		if e.Died {
			t.Error("no death happened")
		}
	})

	t.Run("death truncates the episode and flags died", func(t *testing.T) {
		s := mkState()
		s.PosHistory[5] = []posSample{
			{T: 2000, X: outX, Y: outY},
			{T: 2001, X: inX, Y: inY},
			// corpse frozen in zone until the respawn jump at 2040
			{T: 2010, X: inX, Y: inY},
			{T: 2040, X: outX, Y: outY},
		}
		s.Players[5].DeathEvents = []DeathEvent{{Time: 2005}}
		entries := detectHgEntries(s)
		if len(entries[5]) != 1 {
			t.Fatalf("episodes: got %d, want 1", len(entries[5]))
		}
		e := entries[5][0]
		if !e.Died || e.DurationSec != 4 {
			t.Errorf("truncated episode: got died=%v dur=%v, want died=true dur=4", e.Died, e.DurationSec)
		}
	})

	t.Run("kills, building damage and allies land in the outcome window", func(t *testing.T) {
		s := mkState()
		s.PosHistory[5] = []posSample{
			{T: 3000, X: outX, Y: outY},
			{T: 3001, X: inX, Y: inY},
			{T: 3010, X: inX, Y: inY},
			{T: 3011, X: outX, Y: outY},
		}
		s.Players[5].KillEvents = []KillEvent{{Time: 3003}, {Time: 3019}, {Time: 3025}} // 3025 is past exit+10
		s.Players[5].BuildingDamageTimes = []bdEvent{{T: 3004, Dmg: 350}, {T: 3030, Dmg: 999}}
		s.PosHistory[6] = []posSample{{T: 3000, X: inX + 5, Y: inY}}   // ally on the plateau
		s.PosHistory[7] = []posSample{{T: 3000, X: 150, Y: 150}}      // ally far away
		s.PosHistory[0] = []posSample{{T: 3000, X: inX, Y: inY}}      // enemy — never listed
		entries := detectHgEntries(s)
		if len(entries[5]) != 1 {
			t.Fatalf("episodes: got %d, want 1", len(entries[5]))
		}
		e := entries[5][0]
		if e.Kills != 2 {
			t.Errorf("kills: got %d, want 2", e.Kills)
		}
		if e.BuildingDamage != 350 {
			t.Errorf("buildingDamage: got %d, want 350", e.BuildingDamage)
		}
		if len(e.AlliesNearby) != 1 || e.AlliesNearby[0] != 6 {
			t.Errorf("alliesNearby: got %v, want [6]", e.AlliesNearby)
		}
	})
}

// v4.6.0: plateau zone geometry — the measured tier-3 cells must be inside
// their base zone, ramp-bottom lane points outside, and tower-derived
// thresholds must match the fallback constants on the reference map.
func TestHgZones(t *testing.T) {
	zones := deriveHgZones(map[string][2]float64{
		"dota_goodguys_tower3_bot": {96, 80},
		"dota_goodguys_tower3_mid": {90, 94},
		"dota_goodguys_tower3_top": {76, 100},
		"dota_badguys_tower3_bot":  {176, 150},
		"dota_badguys_tower3_mid":  {160, 156},
		"dota_badguys_tower3_top":  {154, 172},
	})
	if zones != (hgZones{radEdge: 178, radMid: 186, direEdge: 324, direMid: 314}) {
		t.Fatalf("derived zones differ from reference: %+v", zones)
	}
	cases := []struct {
		x, y    float64
		radBase bool
		want    bool
		name    string
	}{
		{96, 80, true, true, "radiant t3 bot on plateau"},
		{90, 94, true, true, "radiant t3 mid on the nose"},
		{80, 86, true, true, "radiant fort"},
		{102, 80, true, false, "bot lane below the ramp"},
		{96, 96, true, false, "mid ramp bottom"},
		{176, 150, false, true, "dire t3 bot on plateau"},
		{160, 156, false, true, "dire t3 mid on the nose"},
		{170, 166, false, true, "dire fort"},
		{140, 174, false, false, "top lane before dire ramp"},
		{140, 140, false, false, "river"},
	}
	for _, c := range cases {
		if got := zones.inBase(c.x, c.y, c.radBase); got != c.want {
			t.Errorf("%s (%v,%v): inBase=%v, want %v", c.name, c.x, c.y, got, c.want)
		}
	}
	// No towers at all → fallback constants keep the detector alive.
	if deriveHgZones(map[string][2]float64{}) != (hgZones{radEdge: 178, radMid: 186, direEdge: 324, direMid: 314}) {
		t.Error("fallback thresholds broken")
	}
}

// Dead lanes (parser gaps, heavy smokes) must still yield a permutation —
// farm rank, crude but never a duplicate.
func TestAssignPositionsDeadLanesPermutation(t *testing.T) {
	state := &ParserState{}
	for i := 0; i < 10; i++ {
		state.Players[i] = &PlayerState{IsRadiant: i < 5, NetWorth: 1000 * (i + 1)}
	}
	lanes := map[int]string{}
	for i := 0; i < 10; i++ {
		lanes[i] = "unknown"
	}
	pos := assignPositions(state, lanes)
	for _, team := range [][]int{{0, 1, 2, 3, 4}, {5, 6, 7, 8, 9}} {
		seen := map[int]bool{}
		for _, idx := range team {
			seen[pos[idx]] = true
		}
		if len(seen) != 5 {
			t.Errorf("team %v: positions are not a permutation: %v", team, pos)
		}
	}
}
