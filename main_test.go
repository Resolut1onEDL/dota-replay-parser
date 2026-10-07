package main

// Regression test for team-per-player assignment.
// Ensures parser output isRadiant/isVictory matches OpenDota ground truth
// (the source of truth, not the previous parser output).
//
// Reproduces the bug from match 8788500456 where Radiant/Dire was swapped
// for several players because the parser hardcoded `IsRadiant: i < 5`.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dotabuff/manta/dota"
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

// A unit that is not a hero must not resolve to a player. Live case: match
// 9032897977, a 1v1 lobby (Luna vs Puck) with slots 2–9 empty (heroId 0). A
// Radiant creep's last hit on Puck resolved to heroId 0 → slot 2, which came
// out as Hero_0 with 1 kill, 43 courier casts and 1.6s of stuns.
func TestHeroNameToPlayerIndexEmptySlots(t *testing.T) {
	state := &ParserState{}
	for i := range state.Players {
		state.Players[i] = &PlayerState{}
	}
	state.Players[0].HeroID = 48 // Luna
	state.Players[1].HeroID = 13 // Puck

	cases := []struct {
		in   string
		want int
	}{
		{"npc_dota_hero_luna", 0},
		{"npc_dota_hero_puck", 1},
		{"npc_dota_creep_goodguys_melee", -1},
		{"npc_dota_courier", -1},
		{"npc_dota_goodguys_tower1_mid", -1},
		{"npc_dota_hero_axe", -1}, // a hero nobody picked
		{"", -1},
	}
	for _, c := range cases {
		if got := heroNameToPlayerIndex(c.in, state); got != c.want {
			t.Errorf("heroNameToPlayerIndex(%q) = %d, want %d", c.in, got, c.want)
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

// rolePlayer builds a PlayerState with the fields assignPositions reads:
// last hits at minute 10 and over the match, wards placed, final net worth.
func rolePlayer(radiant bool, lh10, lh, wards, nw int) *PlayerState {
	ps := &PlayerState{HeroID: 1, IsRadiant: radiant, LastHits: lh, NetWorth: nw}
	for m := 1; m <= 30; m++ {
		cur := lh10 * m / 10
		if m > 10 {
			cur = lh10 + (lh-lh10)*(m-10)/20
		}
		ps.MinuteSnapshots = append(ps.MinuteSnapshots, MinuteSnapshot{LH: cur})
	}
	for w := 0; w < wards; w++ {
		ps.Wards = append(ps.Wards, WardEvent{Time: float64(60 * (w + 1))})
	}
	return ps
}

func checkPermutation(t *testing.T, pos map[int]int) {
	t.Helper()
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

// assignPositions must ALWAYS emit a clean 1..5 permutation per team — the
// bucket-per-lane version before 4.6 could not: a safe-lane trilane produced
// 1+5+5 and no pos-4 (live case: match 8919464063, Dire [5,1,3,5,2] — the
// roaming four and the hard five both labelled 5).
func TestAssignPositionsTrilanePermutation(t *testing.T) {
	state := &ParserState{}
	// Dire mirrors Даня's game: trilane on safe (farming Lifestealer, roaming
	// Pudge, warding Lion), SF mid, Rubick alone on off and farming it.
	state.Players[0] = rolePlayer(false, 55, 300, 0, 24000) // Lifestealer, safe
	state.Players[1] = rolePlayer(false, 8, 60, 6, 10000)   // Pudge, safe (roams)
	state.Players[2] = rolePlayer(false, 4, 40, 25, 7000)   // Lion, safe
	state.Players[3] = rolePlayer(false, 60, 280, 1, 22000) // SF, mid
	state.Players[4] = rolePlayer(false, 30, 150, 2, 9600)  // Rubick, off solo
	// Radiant: an ordinary 2-1-2.
	state.Players[5] = rolePlayer(true, 60, 320, 0, 26000) // carry, safe
	state.Players[6] = rolePlayer(true, 5, 40, 30, 8000)   // hard support, safe
	state.Players[7] = rolePlayer(true, 55, 260, 2, 23000) // mid
	state.Players[8] = rolePlayer(true, 40, 200, 0, 18000) // offlaner
	state.Players[9] = rolePlayer(true, 10, 70, 15, 11000) // soft support, off

	lanes := map[int]string{
		0: "safe", 1: "safe", 2: "safe", 3: "mid", 4: "off",
		5: "safe", 6: "safe", 7: "mid", 8: "off", 9: "off",
	}
	pos := assignPositions(state, lanes, 1800)

	want := map[int]int{
		0: 1, // the farmer of the trilane → carry
		3: 2,
		4: 3, // alone on off and farming it → offlaner
		1: 4, // two supports in one lane: the one who farmed more is the four
		2: 5,
		5: 1, 6: 5, 7: 2, 8: 3, 9: 4,
	}
	for idx, p := range want {
		if pos[idx] != p {
			t.Errorf("player %d: got pos %d, want %d", idx, pos[idx], p)
		}
	}
	checkPermutation(t, pos)
}

// v4.7.5 regression, match 9029043679 (real numbers). The 4.7.4 rule let a
// lane's net worth at mid-game pick its core: Zeus out-earned the Io carry by
// kills and became pos 1 with Io pos 4; Dire's lone warding Lion became the
// carry and the farming Monkey King (lane "jungle") the leftover pos 4.
func TestAssignPositionsLastHitsBeatKillGold(t *testing.T) {
	state := &ParserState{}
	// lh10, lh, wards (max of placed and bought), final net worth
	state.Players[0] = rolePlayer(true, 39, 213, 3, 13109)   // Sniper, mid
	state.Players[1] = rolePlayer(true, 42, 100, 0, 9069)    // Magnus, off
	state.Players[2] = rolePlayer(true, 3, 148, 26, 12663)   // Earthshaker, off
	state.Players[3] = rolePlayer(true, 21, 167, 5, 11820)   // Io, safe
	state.Players[4] = rolePlayer(true, 9, 97, 22, 12984)    // Zeus, safe
	state.Players[5] = rolePlayer(false, 52, 268, 0, 25448)  // Monkey King, jungle
	state.Players[6] = rolePlayer(false, 38, 290, 3, 25549)  // Nature's Prophet, off
	state.Players[7] = rolePlayer(false, 2, 62, 38, 11600)   // Shadow Shaman, off
	state.Players[8] = rolePlayer(false, 6, 42, 34, 11083)   // Lion, safe
	state.Players[9] = rolePlayer(false, 33, 158, 1, 16813)  // Primal Beast, mid
	lanes := map[int]string{
		0: "mid", 1: "off", 2: "off", 3: "safe", 4: "safe",
		5: "jungle", 6: "off", 7: "off", 8: "safe", 9: "mid",
	}
	pos := assignPositions(state, lanes, 2069)
	want := map[int]int{
		0: 2, 1: 3, 2: 4, 3: 1, 4: 5,
		5: 1, 6: 3, 7: 4, 8: 5, 9: 2,
	}
	for idx, p := range want {
		if pos[idx] != p {
			t.Errorf("player %d: got pos %d, want %d", idx, pos[idx], p)
		}
	}
	checkPermutation(t, pos)
}

// A ward BUYER counts as a support even when the placements were dropped
// (wards placed by a teammate after a drop are credited to the placer).
func TestAssignPositionsCountsWardPurchases(t *testing.T) {
	state := &ParserState{}
	for i := 0; i < 10; i++ {
		state.Players[i] = rolePlayer(i < 5, 30, 150, 0, 15000-i*100)
	}
	// Radiant: player 4 farmed like a core but bought 30 wards.
	for w := 0; w < 30; w++ {
		state.Players[4].ItemPurchases = append(state.Players[4].ItemPurchases, ItemPurchase{ItemName: "item_ward_observer"})
	}
	state.Players[3] = rolePlayer(true, 5, 30, 0, 6000)
	lanes := map[int]string{0: "safe", 1: "mid", 2: "off", 3: "off", 4: "safe", 5: "safe", 6: "mid", 7: "off", 8: "off", 9: "safe"}
	pos := assignPositions(state, lanes, 1800)
	if pos[4] < 4 {
		t.Errorf("ward buyer: got pos %d, want a support slot", pos[4])
	}
	checkPermutation(t, pos)
}

// A 1v1 (live case 8999975429: KotL vs Ember, eight empty slots) must not
// hand the empty slots positions past 5 — they belong to no team.
func TestAssignPositionsOneVsOne(t *testing.T) {
	state := &ParserState{}
	for i := 0; i < 10; i++ {
		state.Players[i] = &PlayerState{IsRadiant: false} // empty slot, HeroID 0
	}
	state.Players[0] = rolePlayer(true, 71, 201, 2, 14452)
	state.Players[5] = rolePlayer(false, 8, 10, 1, 2637)
	pos := assignPositions(state, map[int]string{0: "mid", 5: "mid"}, 1260)
	for i, p := range pos {
		if p < 1 || p > 5 {
			t.Errorf("player %d: position %d outside 1..5", i, p)
		}
	}
	if pos[0] != 1 || pos[5] != 1 {
		t.Errorf("the two heroes: got %d/%d, want 1/1", pos[0], pos[5])
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

	// Live case 9006039853: Snapfire pulls the top camp at 558.7 (Dire wave
	// there 562-582.2), the camp is cleared by 577.6 and respawns at 10:00;
	// Spirit Breaker pokes the fresh camp at 600.7 onto the passing Dire
	// wave (Dire run 591.7-617.4, Radiant run 605.8-637.8 — two episodes
	// with the SAME poke time) and two Dire creeps die at 607-608. The
	// result used to flip between runs: the tie came out of the
	// CampWaveRuns map in random order, and the 591.7 episode, if merged
	// first, swallowed SB's pull into Snapfire's.
	t.Run("same-poke tie is deterministic and a later poke is a new pull", func(t *testing.T) {
		s := mkState()
		s.CampSpawners = [][2]float64{{96, 164}}
		for k := 0; k < 24; k++ {
			s.CampSpawners = append(s.CampSpawners, [2]float64{20 + 20*float64(k%6), 20 + 20*float64(k/6)})
		}
		s.CampWaveRuns[0] = &campRuns{
			Rad:  []waveRun{{T0: 605.8, T1: 637.8}},
			Dire: []waveRun{{T0: 562, T1: 582.2}, {T0: 591.7, T1: 617.4}},
		}
		s.HeroNeutHits = []neutHit{
			{T: 558.7, Player: 9, X: 94, Y: 168, Species: "harpy_scout"},
			{T: 600.7, Player: 3, X: 100, Y: 164, Species: "gnoll_assassin"},
		}
		s.NeutDeletions = []neutDeletion{
			{T: 575, X: 96, Y: 164}, {T: 576.3, X: 96, Y: 164}, {T: 577.6, X: 96, Y: 164},
			{T: 607.6, X: 96, Y: 166},
		}
		s.PullDeaths = []pullDeathRec{
			{T: 607.2, Radiant: false, CreepDied: true},
			{T: 608.5, Radiant: false, CreepDied: true},
		}
		// Decoy pulls at 24 far-away camps: enough episodes (>12) that the
		// sort leaves insertion-sort territory, and enough map keys that the
		// iteration order varies between calls.
		for ci := 1; ci < len(s.CampSpawners); ci++ {
			t0 := 100 + 20*float64(ci)
			cx, cy := s.CampSpawners[ci][0], s.CampSpawners[ci][1]
			s.CampWaveRuns[ci] = &campRuns{Rad: []waveRun{{T0: t0, T1: t0 + 15}}}
			s.HeroNeutHits = append(s.HeroNeutHits, neutHit{T: t0 - 2, Player: 4, X: cx, Y: cy, Species: "kobold"})
			s.NeutDeletions = append(s.NeutDeletions, neutDeletion{T: t0 + 10, X: cx, Y: cy})
		}

		first := detectPulls(s)
		for run := 0; run < 200; run++ {
			if got := detectPulls(s); !reflect.DeepEqual(got, first) {
				t.Fatalf("run %d differs:\n got %+v\nwant %+v", run, got, first)
			}
		}
		if len(first[3]) != 1 {
			t.Fatalf("Spirit Breaker 600.7 pull: got %+v, want one", first[3])
		}
		if ev := first[3][0]; ev.Time != 600.7 || ev.CreepsDied != 2 || !ev.OntoEnemyWave {
			t.Errorf("SB pull: %+v, want t=600.7 creepsDied=2 ontoEnemyWave", ev)
		}
		if len(first[9]) != 1 || first[9][0].Time != 558.7 || first[9][0].CreepsDied != 0 {
			t.Errorf("Snapfire pull: %+v, want one at 558.7 with creepsDied=0", first[9])
		}
		if len(first[4]) != len(s.CampSpawners)-1 {
			t.Errorf("decoy pulls: got %d, want %d", len(first[4]), len(s.CampSpawners)-1)
		}
	})

	// Live case 9016805053: Doom (Radiant) pokes at 498.2 while the Dire wave
	// walks past the camp (489.9-500.8, no losses); his own wave then parks
	// (505.2-528.8) and loses a creep at 511.1. Both runs share the poke —
	// the merged pull is a normal pull that cost 1 creep, not "onto enemy".
	t.Run("merged pull direction follows the wave that lost creeps", func(t *testing.T) {
		s := mkState()
		s.CampSpawners = [][2]float64{{90, 158}}
		s.CampWaveRuns[0] = &campRuns{
			Rad:  []waveRun{{T0: 505.2, T1: 528.8}},
			Dire: []waveRun{{T0: 489.9, T1: 500.8}},
		}
		s.HeroNeutHits = []neutHit{{T: 498.2, Player: 0, X: 90, Y: 160, Species: "satyr_trickster"}}
		s.NeutDeletions = []neutDeletion{{T: 515, X: 90, Y: 158}}
		s.PullDeaths = []pullDeathRec{{T: 511.1, Radiant: true, CreepDied: true}}
		pulls := detectPulls(s)
		if len(pulls[0]) != 1 {
			t.Fatalf("Doom pulls: got %+v, want one", pulls[0])
		}
		if ev := pulls[0][0]; ev.Time != 498.2 || ev.CreepsDied != 1 || ev.OntoEnemyWave {
			t.Errorf("Doom pull: %+v, want t=498.2 creepsDied=1 ontoEnemyWave=false", ev)
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

// v4.7.3: combat-log entries made by a player's illusions carry the hero's
// name. Match 9014833909: Bane's two scepter illusions repeated each Fiend's
// Grip (24 casts logged, 10 pressed). While the real hero is alive (lifeState
// 0) they are echoes; a dead Vengeful Spirit's scepter illusion (lifeState
// 1/2) is still the player's hands.
func TestIllusionEcho(t *testing.T) {
	yes, no := true, false
	s := &ParserState{}
	s.LifePrev[3] = 2 // dead
	s.LifePrev[4] = 1 // dying
	cases := []struct {
		name     string
		illusion *bool
		player   int
		want     bool
	}{
		{"illusion of a living hero", &yes, 0, true},
		{"the hero itself", &no, 0, false},
		{"flag absent", nil, 0, false},
		{"illusion of a dead hero", &yes, 3, false},
		{"illusion of a dying hero", &yes, 4, false},
	}
	for _, c := range cases {
		m := &dota.CMsgDOTACombatLogEntry{IsAttackerIllusion: c.illusion}
		if got := s.illusionEcho(m, c.player); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// v4.7.4: final GPM is total earned gold over the minutes since the horn, as
// Valve's gold_per_min. Match 9023007039, slot 0 (21.84 min): 11977 earned at
// the game's end → 548, OpenDota says 548; the old net-worth proxy (12140) said
// 555. A replay without the team-data field keeps that proxy.
func TestFinalGPM(t *testing.T) {
	if got := finalGPM(11977, 12140, 1310.37); got != 548 {
		t.Errorf("total earned gold: got %d, want 548", got)
	}
	if got := finalGPM(0, 12140, 1310.37); got != 555 {
		t.Errorf("net-worth fallback: got %d, want 555", got)
	}
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
// the farm split alone, crude but never a duplicate.
func TestAssignPositionsDeadLanesPermutation(t *testing.T) {
	state := &ParserState{}
	for i := 0; i < 10; i++ {
		state.Players[i] = &PlayerState{HeroID: i + 1, IsRadiant: i < 5, NetWorth: 1000 * (i + 1)}
	}
	lanes := map[int]string{}
	for i := 0; i < 10; i++ {
		lanes[i] = "unknown"
	}
	pos := assignPositions(state, lanes, 1800)
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

// detectLane: plurality vote of per-sample zones, not the coordinate mean.
// The mean erased a zoned offlaner's lane (match 8921261987): minutes 1-5 in
// the lane corridor, 6-10 pushed into jungle/roam — the average landed in the
// central jungle box while the vote says "lane".
func TestDetectLaneVoteBeatsMean(t *testing.T) {
	var pos []struct{ X, Y float64 }
	// 20% skip window eats the head — pad it with lane samples too.
	for i := 0; i < 150; i++ {
		pos = append(pos, struct{ X, Y float64 }{X: 176, Y: 96}) // dire off corridor (bottom)
	}
	for i := 0; i < 80; i++ {
		pos = append(pos, struct{ X, Y float64 }{X: 135, Y: 170}) // jungle box (off the mid diagonal)
	}
	for i := 0; i < 60; i++ {
		pos = append(pos, struct{ X, Y float64 }{X: 190, Y: 125}) // roam (outside boxes)
	}
	// Dire bottom = off lane. The mean of this mix drifts into "jungle" —
	// detectLane (roles) keeps saying so, and the REPORTING second opinion
	// restores the lane that was stood.
	if got := detectLane(pos, false); got != "jungle" {
		t.Errorf("roles map must keep the mean verdict: got %q, want \"jungle\"", got)
	}
	if got := detectErasedLane(pos, false); got != "off" {
		t.Errorf("zoned offlaner report lane: got %q, want \"off\"", got)
	}
}

func TestDetectLanePureJunglerStaysJungle(t *testing.T) {
	var pos []struct{ X, Y float64 }
	for i := 0; i < 200; i++ {
		pos = append(pos, struct{ X, Y float64 }{X: 135, Y: 170}) // off the mid diagonal
	}
	if got := detectLane(pos, true); got != "jungle" {
		t.Errorf("pure jungler: got %q, want \"jungle\"", got)
	}
	if got := detectErasedLane(pos, true); got != "" {
		t.Errorf("pure jungler must get NO restored lane, got %q", got)
	}
}

// v4.7.0: smoke routes. The spatial story of a smoke is assembled in
// detectSmokeEvents from PosHistory + MODIFIER_REMOVE events: activation
// centroid, per-participant path (thinned), per-participant buff end.
func TestDetectSmokeRoutes(t *testing.T) {
	mkState := func() *ParserState {
		s := &ParserState{}
		s.Players[0] = &PlayerState{IsRadiant: true}
		s.Players[1] = &PlayerState{IsRadiant: true}
		// Both walk from (100,150) towards (140,110), 1 sample/s, 580..650.
		for i := 0; i <= 70; i++ {
			tm := 580.0 + float64(i)
			s.PosHistory[0] = append(s.PosHistory[0], posSample{T: tm, X: 100 + float64(i), Y: 150 - float64(i)})
			s.PosHistory[1] = append(s.PosHistory[1], posSample{T: tm, X: 102 + float64(i), Y: 148 - float64(i)})
		}
		s.SmokeModifierAdds = []SmokeModifierAdd{
			{Time: 600, PlayerIdx: 0},
			{Time: 601.5, PlayerIdx: 1},
		}
		return s
	}

	t.Run("routes, ends and activation centroid", func(t *testing.T) {
		s := mkState()
		s.SmokeModifierRemoves = []SmokeModifierAdd{
			{Time: 630, PlayerIdx: 0},
			{Time: 640, PlayerIdx: 1},
		}
		evs := detectSmokeEvents(s)
		if len(evs) != 1 {
			t.Fatalf("events = %d, want 1", len(evs))
		}
		ev := evs[0]
		// Centroid at t=600: p0 at (120,130), p1 at (122,128) → (121,129).
		if ev.X != 121 || ev.Y != 129 {
			t.Errorf("activation = (%v,%v), want (121,129)", ev.X, ev.Y)
		}
		if ev.EndTime != 640 {
			t.Errorf("event endTime = %v, want 640 (last participant)", ev.EndTime)
		}
		if len(ev.Routes) != 2 {
			t.Fatalf("routes = %d, want 2", len(ev.Routes))
		}
		r0 := ev.Routes[0]
		if r0.Idx != 0 || r0.EndTime != 630 {
			t.Errorf("route0 idx=%d end=%v, want 0/630", r0.Idx, r0.EndTime)
		}
		// Position at buff end t=630 (i=50): (150,100).
		if r0.EndX != 150 || r0.EndY != 100 {
			t.Errorf("route0 end pos = (%v,%v), want (150,100)", r0.EndX, r0.EndY)
		}
		if len(r0.Path) == 0 {
			t.Fatal("route0 path empty")
		}
		first, last := r0.Path[0], r0.Path[len(r0.Path)-1]
		if first.T < 585 || last.T > 630 {
			t.Errorf("path spans [%v..%v], want within [585..630]", first.T, last.T)
		}
		for i := 1; i < len(r0.Path); i++ {
			if r0.Path[i].T-r0.Path[i-1].T < 1.8 {
				t.Fatalf("path not thinned: dt=%v at %d", r0.Path[i].T-r0.Path[i-1].T, i)
			}
		}
	})

	// v4.8.1: the smoke both teams press on their way out of the base, before the creeps. Its life is on the
	// negative side of the clock, where "EndTime > 0" used to mean "no end was found" — and where
	// PosHistory had no samples at all, so the event came out without an activation point.
	t.Run("a smoke pressed before the horn keeps its point, its route and its end", func(t *testing.T) {
		s := &ParserState{}
		s.Players[0] = &PlayerState{IsRadiant: true}
		s.Players[1] = &PlayerState{IsRadiant: true}
		for i := 0; i <= 90; i++ { // the pregame: out of the fountain, towards the lanes
			tm := -90.0 + float64(i)
			s.PosHistory[0] = append(s.PosHistory[0], posSample{T: tm, X: 70 + float64(i)/3, Y: 70 + float64(i)/3})
			s.PosHistory[1] = append(s.PosHistory[1], posSample{T: tm, X: 72 + float64(i)/3, Y: 68 + float64(i)/3})
		}
		s.SmokeModifierAdds = []SmokeModifierAdd{{Time: -73, PlayerIdx: 0}, {Time: -72.5, PlayerIdx: 1}}
		s.SmokeModifierRemoves = []SmokeModifierAdd{{Time: -40, PlayerIdx: 0}, {Time: -38, PlayerIdx: 1}}
		evs := detectSmokeEvents(s)
		if len(evs) != 1 {
			t.Fatalf("events = %d, want 1", len(evs))
		}
		ev := evs[0]
		if ev.GameTime != -73 {
			t.Errorf("gameTime = %v, want -73", ev.GameTime)
		}
		if ev.X == 0 || ev.Y == 0 {
			t.Errorf("activation = (%v,%v), want a real point, not the missing one that hid the smoke", ev.X, ev.Y)
		}
		if ev.EndTime != -38 {
			t.Errorf("event endTime = %v, want -38 (the last participant's, negative)", ev.EndTime)
		}
		r0 := ev.Routes[0]
		if r0.EndTime != -40 || r0.EndX == 0 {
			t.Errorf("route0 end = %v at (%v,%v), want -40 with a position", r0.EndTime, r0.EndX, r0.EndY)
		}
		if len(r0.Path) < 2 {
			t.Fatalf("route0 path has %d points, want a walked route", len(r0.Path))
		}
		if last := r0.Path[len(r0.Path)-1]; last.T > -40 {
			t.Errorf("path runs past the buff end: %v", last.T)
		}
	})

	t.Run("no REMOVE found: path capped, no endTime", func(t *testing.T) {
		s := mkState()
		evs := detectSmokeEvents(s)
		if len(evs) != 1 {
			t.Fatalf("events = %d, want 1", len(evs))
		}
		r0 := evs[0].Routes[0]
		if r0.EndTime != 0 || r0.EndX != 0 {
			t.Errorf("endTime/endX = %v/%v, want absent (0)", r0.EndTime, r0.EndX)
		}
		last := r0.Path[len(r0.Path)-1]
		if last.T > 650 {
			t.Errorf("uncapped path: last T = %v", last.T)
		}
	})
}

// v4.7.0: deward vs expiry. The ward entity outlives its combat-log death by
// the corpse decay (~6.3-8.0s measured), so a kill whose (deletion - death)
// lag falls in [5.0, 9.5]s stamps Killed=true and is consumed exactly once;
// anything outside the window is a natural expiry.
func TestFinalizeWardKilled(t *testing.T) {
	mk := func(pending []pendingDeward) *ParserState {
		s := &ParserState{ActiveWards: make(map[int32]*activeWard)}
		s.Players[3] = &PlayerState{Wards: []WardEvent{{Time: 100, Type: 0}, {Time: 200, Type: 0}}}
		s.ActiveWards[42] = &activeWard{playerIdx: 3, sliceIdx: 0, start: 100}
		s.ActiveWards[43] = &activeWard{playerIdx: 3, sliceIdx: 1, start: 200}
		s.PendingDewards = pending
		return s
	}

	t.Run("matched kill marks the ward and is consumed", func(t *testing.T) {
		s := mk([]pendingDeward{{t: 353, wardType: 0}}) // deletion lags by 7s
		finalizeWard(s, 42, 360)
		if !s.Players[3].Wards[0].Killed {
			t.Error("ward not marked killed")
		}
		finalizeWard(s, 43, 361) // second deletion: the pending record is spent
		if s.Players[3].Wards[1].Killed {
			t.Error("one combat-log kill claimed two wards")
		}
	})

	t.Run("type mismatch does not match", func(t *testing.T) {
		s := mk([]pendingDeward{{t: 353, wardType: 1}}) // sentry kill, observer ward
		finalizeWard(s, 42, 360)
		if s.Players[3].Wards[0].Killed {
			t.Error("sentry kill claimed an observer ward")
		}
	})

	t.Run("outside the corpse-lag window = natural expiry", func(t *testing.T) {
		for _, killT := range []float64{300, 347, 356} { // lags 60 / 13 / 4
			s := mk([]pendingDeward{{t: killT, wardType: 0}})
			finalizeWard(s, 42, 360)
			if s.Players[3].Wards[0].Killed {
				t.Errorf("a kill with lag %v claimed the ward", 360-killT)
			}
		}
	})
}

// Regression for v4.7.1: skillBuild held other heroes' abilities (match
// 8983627958: Terrorblade got nevermore_frenzy, rubick_spellsteal, ...;
// match 9008951145: SF and Pudge swapped level-ups). Level-ups used to be
// keyed by ability NAME and credited to the first player who had that name;
// now only an in-place level rise of an entity claimed by the owner's real
// hero counts. (Illusion hero entities are skipped before this point.)
func TestSkillBuildOwnership(t *testing.T) {
	names := func(s *ParserState, p int) []string {
		out := []string{}
		for _, su := range s.Players[p].SkillBuild {
			out = append(out, fmt.Sprintf("%s:%d", su.AbilityName, su.Level))
		}
		return out
	}

	t.Run("own ability levels up through either path, once", func(t *testing.T) {
		s := NewParserState(nil)
		s.observeHeroAbility(0, 100, "nevermore_shadowraze1", 0, false, 0)
		s.observeAbilityLevel(100, "nevermore_shadowraze1", 1, 10) // ability entity first
		s.observeHeroAbility(0, 100, "nevermore_shadowraze1", 1, false, 10)
		s.observeHeroAbility(0, 100, "nevermore_shadowraze1", 2, false, 60) // hero entity first
		s.observeAbilityLevel(100, "nevermore_shadowraze1", 2, 60)
		if got := names(s, 0); fmt.Sprint(got) != "[nevermore_shadowraze1:1 nevermore_shadowraze1:2]" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("level-up goes to the entity's owner, not the first same-named player", func(t *testing.T) {
		s := NewParserState(nil)
		// Player 0 holds a copy of pudge_meathook (e.g. morphed) at level 1.
		s.observeHeroAbility(0, 300, "pudge_meathook", 1, false, 0)
		// Player 3 is Pudge.
		s.observeHeroAbility(3, 200, "pudge_meathook", 1, false, 0)
		s.observeAbilityLevel(200, "pudge_meathook", 2, 100)
		if got := names(s, 0); len(got) != 0 {
			t.Errorf("player 0 got Pudge's level-up: %v", got)
		}
		if got := names(s, 3); fmt.Sprint(got) != "[pudge_meathook:2]" {
			t.Errorf("player 3 got %v", got)
		}
	})

	t.Run("a new entity at a higher level is a baseline, not a skill point", func(t *testing.T) {
		s := NewParserState(nil)
		// Doom devours a creep with ability level 1, later another at level 3.
		s.observeHeroAbility(0, 400, "black_dragon_fireball", 1, false, 300)
		s.observeHeroAbility(0, 401, "black_dragon_fireball", 3, false, 900)
		if got := names(s, 0); len(got) != 0 {
			t.Errorf("devoured ability recorded as skill points: %v", got)
		}
	})

	t.Run("stolen spells never count, even when re-stolen at a higher level", func(t *testing.T) {
		s := NewParserState(nil)
		s.observeHeroAbility(0, 500, "lina_dragonslave", 2, true, 400)
		s.observeHeroAbility(0, 500, "lina_dragonslave", 4, true, 1200) // Rubick reuses the entity
		s.observeAbilityLevel(500, "lina_dragonslave", 4, 1200)
		if got := names(s, 0); len(got) != 0 {
			t.Errorf("stolen spell recorded: %v", got)
		}
	})

	t.Run("unclaimed ability entities (illusion copies) are ignored", func(t *testing.T) {
		s := NewParserState(nil)
		s.observeAbilityLevel(600, "earthspirit_rollingboulder", 2, 900)
		for p := 0; p < 10; p++ {
			if got := names(s, p); len(got) != 0 {
				t.Errorf("player %d got %v", p, got)
			}
		}
	})

	t.Run("a recycled entity index is re-baselined for its new ability", func(t *testing.T) {
		s := NewParserState(nil)
		s.observeHeroAbility(0, 700, "nevermore_necromastery", 1, false, 0)
		s.observeHeroAbility(0, 700, "terrorblade_reflection", 2, false, 500)
		s.observeAbilityLevel(700, "nevermore_necromastery", 3, 600)
		if got := names(s, 0); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
}

// Regression: skill points taken before the horn were recorded while
// GameStartTime was still 0, so ActualGameSeconds returned the raw server
// time (seconds since the demo started) instead of a pre-horn time. Match
// 8665357419: Invoker's first point (exort, hero level 1) landed at 200s,
// after quas at 79s. Once m_flGameStartTime arrives (286.4s raw) those early
// entries must be rebased onto the game clock.
func TestPreHornRebaseKeepsFirstSkillPointPreHorn(t *testing.T) {
	// Recorded while the horn was unknown: parked on the pregame epoch (raw 200.5 s).
	s := &ParserState{}
	s.Players[1] = &PlayerState{SkillBuild: []SkillLevelUp{
		{Time: 200.5 - preHornEpoch, AbilityName: "invoker_exort", Level: 1, HeroLevel: 1},
	}}
	s.rebasePreHorn(286.4)
	s.GameStartTime = 286.4
	s.Players[1].SkillBuild = append(s.Players[1].SkillBuild,
		SkillLevelUp{Time: 79.4, AbilityName: "invoker_quas", Level: 1, HeroLevel: 2})

	got := filterSkillBuild(s.Players[1].SkillBuild)
	if len(got) != 2 || got[0].AbilityName != "invoker_exort" || got[1].AbilityName != "invoker_quas" {
		t.Fatalf("order = %+v, want exort then quas", got)
	}
	if d := got[0].Time - (200.5 - 286.4); d > 1e-6 || d < -1e-6 {
		t.Errorf("exort time = %v, want %v (pre-horn)", got[0].Time, 200.5-286.4)
	}
	if got[1].Time != 79.4 {
		t.Errorf("quas time = %v, want 79.4 (recorded after start, untouched)", got[1].Time)
	}
}

// The count reports are built from maps: without an explicit order the JSON
// arrays came out shuffled on every parse of the same demo (39/39 replays),
// and the web app's "top 6 casts" broke count ties by that order.
func TestReportArraysDeterministic(t *testing.T) {
	build := func() *PlayerStats {
		s := NewParserState(nil)
		ps := s.Players[0]
		ps.AbilityCasts = map[string]int{
			"pudge_meat_hook": 30, "pudge_rot": 12, "pudge_dismember": 12,
			"item_tpscroll": 5, "pudge_flesh_heap": 0, "ability_capture": 5,
		}
		ps.DamageByTarget = map[int]*DamageTarget{
			9: {Target: 9, PhysicalDamage: 100}, 5: {Target: 5, MagicalDamage: 50},
			7: {Target: 7, PureDamage: 10}, 6: {Target: 6, PhysicalDamage: 1},
			8: {Target: 8, MagicalDamage: 3},
		}
		ps.ItemUsage = map[string]int{
			"item_tango": 4, "item_flask": 2, "item_clarity": 2,
			"item_blink": 7, "item_bottle": 2, "item_ward_observer": 4,
		}
		return buildMatchOutput(s, 1800).Players[0].Stats
	}
	first := build()
	for run := 0; run < 50; run++ {
		if got := build(); !reflect.DeepEqual(got.AbilityCastReport, first.AbilityCastReport) ||
			!reflect.DeepEqual(got.HeroDamageReport, first.HeroDamageReport) ||
			!reflect.DeepEqual(got.ItemUsed, first.ItemUsed) {
			t.Fatalf("run %d: report order differs between builds of the same state", run)
		}
	}
	var casts, targets, items []string
	for _, a := range first.AbilityCastReport {
		casts = append(casts, fmt.Sprintf("%s=%d", a.AbilityName, a.Count))
	}
	for _, d := range first.HeroDamageReport {
		targets = append(targets, fmt.Sprint(d.Target))
	}
	for _, u := range first.ItemUsed {
		items = append(items, fmt.Sprintf("%s=%d", u.ItemName, u.Count))
	}
	wantCasts := []string{"pudge_meat_hook=30", "pudge_dismember=12", "pudge_rot=12",
		"ability_capture=5", "item_tpscroll=5", "pudge_flesh_heap=0"}
	wantTargets := []string{"5", "6", "7", "8", "9"}
	wantItems := []string{"item_blink=7", "item_tango=4", "item_ward_observer=4",
		"item_bottle=2", "item_clarity=2", "item_flask=2"}
	if !reflect.DeepEqual(casts, wantCasts) {
		t.Errorf("abilityCastReport: %v, want %v", casts, wantCasts)
	}
	if !reflect.DeepEqual(targets, wantTargets) {
		t.Errorf("heroDamageReport targets: %v, want %v", targets, wantTargets)
	}
	if !reflect.DeepEqual(items, wantItems) {
		t.Errorf("itemUsed: %v, want %v", items, wantItems)
	}
}

// A camp whose leader votes tie between two tiers has no tier: the winner
// used to be whichever tier the map iteration handed out first, so
// stackEvents[].campTier flipped between runs (2/39 replays).
func TestCampTiersTieIsUnknown(t *testing.T) {
	s := &ParserState{CampSpawners: [][2]float64{{90, 158}, {158, 88}}}
	s.HeroNeutHits = []neutHit{
		// camp 0: one large leader, one medium leader — a tie
		{T: 100, Player: 8, X: 91, Y: 157, Species: "polar_furbolg_ursa_warrior"},
		{T: 160, Player: 8, X: 90, Y: 158, Species: "mud_golem"},
		// camp 1: 2 medium vs 1 large — medium wins
		{T: 100, Player: 1, X: 158, Y: 88, Species: "mud_golem"},
		{T: 130, Player: 1, X: 158, Y: 89, Species: "ogre_magi"},
		{T: 160, Player: 1, X: 157, Y: 88, Species: "polar_furbolg_ursa_warrior"},
	}
	for run := 0; run < 50; run++ {
		tiers := campTiers(s)
		if tiers[0] != "" || tiers[1] != "medium" {
			t.Fatalf("run %d: tiers %q, want [\"\" \"medium\"]", run, tiers)
		}
	}
}

// Match 9032897977 (league 20335 practice lobby, Luna vs Puck): the first
// CParticleSystem baseline holds m_iServerControlPointAssignments = 255, a
// fixed8 uint8 that manta v1.4.7 read as a varint, so the parse died at tick
// 2200 with "nextByte: insufficient buffer (380 of 379)". Fixed by the
// third_party/manta patch. Replay (zstd under the .dem.bz2 name):
// http://replay151.valve.net/570/9032897977_1496998234.dem.bz2
func TestFixed8ParticleBaselineReplay(t *testing.T) {
	demPath := filepath.Join("test-replays", "9032897977.dem.bz2")
	if _, err := os.Stat(demPath); err != nil {
		t.Skipf("no replay: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "parser")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, demPath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("parser run: %v\n%s", err, stderr.String())
	}
	var got Match
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parser stdout not JSON: %v", err)
	}

	// Ground truth: OpenDota /api/matches/9032897977.
	if got.ID != 9032897977 || got.DurationSeconds != 588 || !got.DidRadiantWin {
		t.Errorf("match: id=%d duration=%d radiantWin=%v, want 9032897977/588/true",
			got.ID, got.DurationSeconds, got.DidRadiantWin)
	}
	want := map[int][5]int{ // heroId → kills, deaths, assists, last hits, level
		48: {5, 0, 0, 79, 10}, // Luna
		13: {0, 6, 0, 1, 3},   // Puck
	}
	for _, p := range got.Players {
		w, ok := want[p.HeroID]
		if !ok {
			continue
		}
		delete(want, p.HeroID)
		if g := [5]int{p.Kills, p.Deaths, p.Assists, p.NumLastHits, p.Level}; g != w {
			t.Errorf("hero %d K/D/A/LH/level = %v, want %v", p.HeroID, g, w)
		}
	}
	for id := range want {
		t.Errorf("hero %d missing from players", id)
	}
}
