// Package retention removes replays that a new patch has made obsolete. It is
// deliberately timid: dry-run unless told otherwise, only coach and pro
// kinds, never a pro game the web still references, and no run may remove
// more than a quarter of what it could remove.
//
// It also removes the free players' games the web parsed ahead of a request
// and nobody opened in 7 days (web specs/2026-10-30-prefetch-active-replays.md):
// only the ids the web's purge route names, only of kind student, dry-run
// unless PrefetchApply, at most PrefetchMax a run.
package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"dota-replay-parser/internal/vault"
)

// Config is one run's setting.
type Config struct {
	Apply      bool
	MaxPercent int // abort when more than this share of the pool would go (default 25)
	OpenDota   string
	WebURL     string
	Token      string // VAULT_SERVICE_TOKEN for the pins route
	HTTP       *http.Client
	Now        func() time.Time
	// PrefetchApply deletes the games the web's purge route names (dry-run otherwise).
	PrefetchApply bool
	// PrefetchMax caps the deletions of one run (default 2000).
	PrefetchMax int
}

// Report is written to _vault/retention/<date>.json.
type Report struct {
	At            string         `json:"at"`
	Apply         bool           `json:"apply"`
	CurrentPatch  int            `json:"current_patch"`
	PreviousPatch int            `json:"previous_patch"`
	Pool          int            `json:"pool"`
	Candidates    int            `json:"candidates"`
	Deleted       int            `json:"deleted"`
	PinnedPro     int            `json:"pinned_pro"`
	ProSkipped    int            `json:"pro_skipped_no_pins,omitempty"`
	ChangedSkip   int            `json:"changed_skipped,omitempty"`
	UnknownPatch  int            `json:"unknown_patch"`
	ResolvedPatch int            `json:"resolved_patch"`
	Aborted       string         `json:"aborted,omitempty"`
	Error         string         `json:"error,omitempty"`
	CandidateIDs  []int64        `json:"candidate_ids,omitempty"`
	Prefetch      PrefetchReport `json:"prefetch"`
}

// PrefetchReport is the part of a run that removes the games parsed ahead.
type PrefetchReport struct {
	Apply      bool   `json:"apply"`
	Listed     int    `json:"listed"`     // ids the web named
	Candidates int    `json:"candidates"` // of them in the vault, kind student
	Deleted    int    `json:"deleted"`
	Error      string `json:"error,omitempty"`
}

type patchEntry struct {
	ID   int       `json:"id"`
	Name string    `json:"name"`
	Date time.Time `json:"date"`
}

func (c Config) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func (c Config) get(ctx context.Context, url, bearer string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, err
}

// patches returns the patch list sorted by id, and the current and previous
// entries: current = the newest dated at or before now.
func (c Config) patches(ctx context.Context) (all []patchEntry, cur, prev patchEntry, err error) {
	base := strings.TrimRight(c.OpenDota, "/")
	if base == "" {
		base = "https://api.opendota.com/api"
	}
	b, code, err := c.get(ctx, base+"/constants/patch", "")
	if err != nil {
		return nil, cur, prev, err
	}
	if code != http.StatusOK {
		return nil, cur, prev, fmt.Errorf("opendota patch list: %d", code)
	}
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, cur, prev, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	now := c.now()
	idx := -1
	for i, p := range all {
		if !p.Date.After(now) {
			idx = i
		}
	}
	if idx < 1 {
		return all, cur, prev, fmt.Errorf("opendota patch list has no current and previous patch")
	}
	return all, all[idx], all[idx-1], nil
}

// pins asks the web for the pro games it still references. ok=false when the
// route is missing or fails: then no pro game is touched.
func (c Config) pins(ctx context.Context) (map[int64]bool, bool) {
	if c.WebURL == "" || c.Token == "" {
		return nil, false
	}
	b, code, err := c.get(ctx, strings.TrimRight(c.WebURL, "/")+"/api/internal/vault/pins", c.Token)
	if err != nil || code != http.StatusOK {
		return nil, false
	}
	var out struct {
		Pro []json.Number `json:"pro"`
	}
	if json.Unmarshal(b, &out) != nil {
		return nil, false
	}
	set := map[int64]bool{}
	for _, n := range out.Pro {
		if id, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			set[id] = true
		}
	}
	return set, true
}

// purgeList asks the web for the games parsed ahead that may go. An error or a
// missing route deletes nothing.
func (c Config) purgeList(ctx context.Context) ([]int64, error) {
	if c.WebURL == "" || c.Token == "" {
		return nil, fmt.Errorf("web url or token missing")
	}
	b, code, err := c.get(ctx, strings.TrimRight(c.WebURL, "/")+"/api/internal/vault/purge", c.Token)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("purge route: %d", code)
	}
	var out struct {
		Prefetch []json.Number `json:"prefetch"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(out.Prefetch))
	for _, n := range out.Prefetch {
		if id, err := strconv.ParseInt(n.String(), 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// purgePrefetch removes the named games that the vault holds as a student's
// game; anything else the list names is left alone.
func purgePrefetch(ctx context.Context, v *vault.Vault, cfg Config) PrefetchReport {
	rep := PrefetchReport{Apply: cfg.PrefetchApply}
	ids, err := cfg.purgeList(ctx)
	if err != nil {
		rep.Error = "purge_list_unavailable: " + err.Error()
		return rep
	}
	rep.Listed = len(ids)
	max := cfg.PrefetchMax
	if max <= 0 {
		max = 2000
	}
	for _, id := range ids {
		if rep.Candidates >= max {
			break
		}
		cur, ok, err := v.Find(ctx, id)
		if err != nil {
			rep.Error = fmt.Sprintf("find %d: %v", id, err)
			return rep
		}
		if !ok || cur.Meta.Kind != vault.KindStudent {
			continue
		}
		rep.Candidates++
		if !cfg.PrefetchApply {
			continue
		}
		if err := v.S.Delete(ctx, vault.ReplayKey(id)); err != nil {
			rep.Error = fmt.Sprintf("delete %d: %v", id, err)
			return rep
		}
		rep.Deleted++
	}
	return rep
}

func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

// startTimes finds, for the wanted ids, the start time in their newest done marker.
func startTimes(ctx context.Context, v *vault.Vault, want map[int64]bool) map[int64]float64 {
	newest := map[int64]string{} // id -> version
	_ = v.S.List(ctx, vault.DoneRoot(), func(o vault.ObjectInfo) error {
		parts := strings.Split(strings.TrimPrefix(o.Key, vault.DoneRoot()), "/")
		if len(parts) != 2 {
			return nil
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || !want[id] {
			return nil
		}
		if cur, ok := newest[id]; !ok || semverLess(cur, parts[0]) {
			newest[id] = parts[0]
		}
		return nil
	})
	out := map[int64]float64{}
	for id, ver := range newest {
		if d, ok, err := v.ReadDone(ctx, ver, id); err == nil && ok && d.StartTime > 0 {
			out[id] = d.StartTime
		}
	}
	return out
}

// Run is one retention pass. It always writes its report; it deletes only
// with cfg.Apply and only when every guard holds.
func Run(ctx context.Context, v *vault.Vault, cfg Config) (Report, error) {
	now := cfg.now()
	rep := Report{At: now.Format(time.RFC3339), Apply: cfg.Apply}
	finish := func(err error) (Report, error) {
		if werr := v.PutJSON(ctx, vault.RetentionKey(now.Format("20060102")), rep); werr != nil && err == nil {
			err = werr
		}
		return rep, err
	}

	// the games parsed ahead go by the web's list, whatever the patch step finds
	rep.Prefetch = purgePrefetch(ctx, v, cfg)

	all, cur, prev, err := cfg.patches(ctx)
	if err != nil {
		rep.Error = "patches_unavailable: " + err.Error()
		return finish(nil) // nothing is deleted; the report says why
	}
	rep.CurrentPatch, rep.PreviousPatch = cur.ID, prev.ID

	ids, err := v.ListIDs(ctx)
	if err != nil {
		return rep, err
	}
	objs, err := v.Objects(ctx, ids)
	if err != nil {
		return rep, err
	}
	pins, havePins := cfg.pins(ctx)

	// patch of a start time: the newest patch dated at or before it
	patchAt := func(sec float64) int {
		t := time.Unix(int64(sec), 0).UTC()
		id := 0
		for _, p := range all {
			if !p.Date.After(t) {
				id = p.ID
			}
		}
		return id
	}

	var pool []vault.Object
	unknown := map[int64]bool{}
	for _, o := range objs {
		switch o.Meta.Kind {
		case vault.KindCoach:
		case vault.KindPro:
			if !havePins {
				rep.ProSkipped++
				continue
			}
			if pins[o.ID] {
				rep.PinnedPro++
				continue
			}
		default:
			continue
		}
		pool = append(pool, o)
		if o.Meta.Patch == "" {
			unknown[o.ID] = true
		}
	}
	rep.Pool = len(pool)

	var times map[int64]float64
	if len(unknown) > 0 {
		times = startTimes(ctx, v, unknown)
	}
	type cand struct {
		id    int64
		kind  vault.Kind
		patch int
	}
	var cands []cand
	for _, o := range pool {
		patch := 0
		if o.Meta.Patch != "" {
			patch, _ = strconv.Atoi(o.Meta.Patch)
		} else if st, ok := times[o.ID]; ok {
			if patch = patchAt(st); patch > 0 {
				rep.ResolvedPatch++
				if cfg.Apply {
					if _, err := v.UpdateMeta(ctx, o.ID, vault.Meta{Kind: o.Meta.Kind, Patch: strconv.Itoa(patch)}); err != nil {
						return rep, err
					}
				}
			}
		}
		if patch == 0 {
			rep.UnknownPatch++
			continue
		}
		if patch < prev.ID {
			cands = append(cands, cand{o.ID, o.Meta.Kind, patch})
		}
	}
	rep.Candidates = len(cands)
	for _, c := range cands {
		rep.CandidateIDs = append(rep.CandidateIDs, c.id)
	}
	if len(rep.CandidateIDs) > 500 {
		rep.CandidateIDs = rep.CandidateIDs[:500]
	}

	max := cfg.MaxPercent
	if max <= 0 {
		max = 25
	}
	if len(pool) > 0 && len(cands)*100 > len(pool)*max {
		rep.Aborted = fmt.Sprintf("needs_force: %d of %d would go (more than %d%%)", len(cands), len(pool), max)
		return finish(nil)
	}
	if cfg.Apply {
		for _, c := range cands {
			// The decision rests on HEADs taken minutes ago: look again, and
			// leave the object alone if it was upgraded or re-dated meanwhile.
			cur, ok, err := v.Find(ctx, c.id)
			if err != nil {
				return finish(fmt.Errorf("recheck %d: %w", c.id, err))
			}
			if !ok {
				continue
			}
			if cur.Meta.Kind != c.kind || (cur.Meta.Patch != "" && cur.Meta.Patch != strconv.Itoa(c.patch)) {
				rep.ChangedSkip++
				continue
			}
			if err := v.S.Delete(ctx, vault.ReplayKey(c.id)); err != nil {
				return finish(fmt.Errorf("delete %d: %w", c.id, err))
			}
			rep.Deleted++
		}
	}
	return finish(nil)
}
