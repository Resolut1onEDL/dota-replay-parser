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
