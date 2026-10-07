package audit

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// FieldStat is one field over all compared matches.
type FieldStat struct {
	Field    string  `json:"field"`
	Group    string  `json:"group"`
	Rows     int     `json:"rows"`
	OK       int     `json:"ok"`
	Rate     float64 `json:"rate"`
	Examples []Check `json:"examples,omitempty"` // the first mismatches
}

const maxExamples = 8

// Aggregate turns the checks of many matches into one line per field, worst first within each group.
func Aggregate(checks []Check) []FieldStat {
	by := map[string]*FieldStat{}
	for _, c := range checks {
		s := by[c.Field]
		if s == nil {
			s = &FieldStat{Field: c.Field, Group: c.Group}
			by[c.Field] = s
		}
		s.Rows++
		if c.OK {
			s.OK++
		} else if len(s.Examples) < maxExamples {
			s.Examples = append(s.Examples, c)
		}
	}
	out := make([]FieldStat, 0, len(by))
	for _, s := range by {
		s.Rate = float64(s.OK) / float64(s.Rows)
		out = append(out, *s)
	}
	order := map[string]int{GroupValve: 0, GroupReplay: 1, GroupConsistency: 2, GroupHeuristic: 3}
	sort.Slice(out, func(i, j int) bool {
		if order[out[i].Group] != order[out[j].Group] {
			return order[out[i].Group] < order[out[j].Group]
		}
		if out[i].Rate != out[j].Rate {
			return out[i].Rate < out[j].Rate
		}
		return out[i].Field < out[j].Field
	})
	return out
}

// Baseline is the lowest match rate each field may have; a field missing from it is not gated.
type Baseline map[string]float64

func LoadBaseline(path string) (Baseline, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Baseline
	return b, json.Unmarshal(raw, &b)
}

// NewBaseline freezes today's rates (rounded down to 0.1 %). Heuristic fields are never gated.
func NewBaseline(stats []FieldStat) Baseline {
	b := Baseline{}
	for _, s := range stats {
		if s.Group == GroupHeuristic {
			continue
		}
		b[s.Field] = math.Floor(s.Rate*1000) / 1000
	}
	return b
}

// Regressions lists the fields that match worse than the baseline allows. A field at 100 % must stay there; any
// other may fall by sampling noise — three standard errors of its rate over this run's rows, at least 2 points
// (a 20-match weekly run has ~180 rows: a 52 % field moves ±11 points by chance).
func Regressions(stats []FieldStat, b Baseline) []string {
	var out []string
	for _, s := range stats {
		base, ok := b[s.Field]
		if !ok {
			continue
		}
		tol := 0.0
		if base < 1 {
			tol = math.Max(0.02, 3*math.Sqrt(base*(1-base)/float64(s.Rows)))
		}
		if s.Rate+1e-9 < base-tol {
			out = append(out, fmt.Sprintf("%s: %.1f%% < %.1f%% (baseline %.1f%% − %.1f)", s.Field, s.Rate*100, (base-tol)*100, base*100, tol*100))
		}
	}
	return out
}

var groupTitle = map[string]string{
	GroupValve:       "Valve's numbers (scoreboard, items, score) — must be exact",
	GroupReplay:      "OpenDota's parse of the same replay — a second opinion",
	GroupConsistency: "The parser against itself",
	GroupHeuristic:   "Heuristics on both sides — information only",
}

// Markdown renders the report: one table per group, then the mismatch examples of every field below 100 %.
func Markdown(title string, matches, parsedByOD int, stats []FieldStat, regressions []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%d matches compared, %d of them parsed by OpenDota (the others have Valve's numbers only).\n\n", title, matches, parsedByOD)
	if regressions != nil {
		if len(regressions) == 0 {
			b.WriteString("Baseline: no field matches worse than allowed.\n\n")
		} else {
			b.WriteString("## Worse than the baseline\n\n")
			for _, r := range regressions {
				fmt.Fprintf(&b, "- %s\n", r)
			}
			b.WriteString("\n")
		}
	}
	group := ""
	for _, s := range stats {
		if s.Group != group {
			group = s.Group
			fmt.Fprintf(&b, "## %s\n\n| field | match | rows |\n|---|---:|---:|\n", groupTitle[group])
		}
		fmt.Fprintf(&b, "| `%s` | %.1f%% | %d/%d |\n", s.Field, s.Rate*100, s.OK, s.Rows)
		if next := nextGroup(stats, s); next {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n## Mismatch examples\n\n")
	for _, s := range stats {
		if len(s.Examples) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### `%s` — %.1f%%\n\n", s.Field, s.Rate*100)
		for _, e := range s.Examples {
			who := ""
			if e.HeroID != 0 {
				who = fmt.Sprintf(" hero %d", e.HeroID)
			}
			note := ""
			if e.Note != "" {
				note = " — " + e.Note
			}
			fmt.Fprintf(&b, "- %d%s: ours %s, theirs %s%s\n", e.MatchID, who, e.Ours, e.Theirs, note)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func nextGroup(stats []FieldStat, s FieldStat) bool {
	for i := range stats {
		if stats[i].Field == s.Field {
			return i+1 == len(stats) || stats[i+1].Group != s.Group
		}
	}
	return false
}
