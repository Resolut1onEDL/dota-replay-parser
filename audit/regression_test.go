package audit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The audit's regression gate on the replays kept in ../test-replays/ (<id>.dem or .dem.bz2 with
// <id>_opendota.json, see its README): every field the baseline holds at 100 % must match on each of them.
// Skipped when no pair is present — the replays are not in git.
func TestAuditAgainstOpenDota(t *testing.T) {
	base, err := LoadBaseline("baseline.json")
	if err != nil {
		t.Skipf("no audit baseline: %v", err)
	}
	dir := filepath.Join("..", "test-replays")
	pairs, _ := filepath.Glob(filepath.Join(dir, "*_opendota.json"))
	var files []string
	for _, p := range pairs {
		id := strings.TrimSuffix(filepath.Base(p), "_opendota.json")
		for _, ext := range []string{".dem", ".dem.bz2"} {
			if _, err := os.Stat(filepath.Join(dir, id+ext)); err == nil {
				files = append(files, id+ext)
				break
			}
		}
	}
	if len(files) == 0 {
		t.Skip("no replay with an OpenDota answer in test-replays/")
	}
	bin := filepath.Join(t.TempDir(), "parser")
	if out, err := exec.Command("go", "build", "-o", bin, "..").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	for _, file := range files {
		id := strings.SplitN(file, ".", 2)[0]
		t.Run(id, func(t *testing.T) {
			out, err := exec.Command(bin, filepath.Join(dir, file)).Output()
			if err != nil {
				t.Fatalf("parser run: %v", err)
			}
			var ours Match
			if err := json.Unmarshal(out, &ours); err != nil {
				t.Fatalf("parser stdout not JSON: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, id+"_opendota.json"))
			if err != nil {
				t.Fatal(err)
			}
			var od ODMatch
			if err := json.Unmarshal(raw, &od); err != nil {
				t.Fatal(err)
			}
			for _, c := range Compare(&ours, &od, Consts{}) {
				if !c.OK && base[c.Field] >= 1 {
					t.Errorf("%s hero %d: ours %s, OpenDota %s %s", c.Field, c.HeroID, c.Ours, c.Theirs, c.Note)
				}
			}
		})
	}
}
