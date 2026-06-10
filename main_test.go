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
