package reparse

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"dota-replay-parser/internal/vault"
)

// PlanItem is one replay of a run.
type PlanItem struct {
	ID   int64      `json:"id"`
	Kind vault.Kind `json:"kind"`
}

// Plan is stored as _vault/runs/<ver>/plan.json (the first attempt of a
// version) and _vault/runs/<ver>/attempt-<stamp>.json (every attempt).
type Plan struct {
	Version   string     `json:"version"`
	Attempt   string     `json:"attempt"`
	StartedAt string     `json:"startedAt"`
	Shards    int        `json:"shards"`
	Items     []PlanItem `json:"items"`
}

// Shard is fnv32a(id) % n: stable, so a resumed run assigns the same replay
// to the same shard whatever has already been done.
func Shard(id int64, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	h.Write([]byte(strconv.FormatInt(id, 10)))
	return int(h.Sum32() % uint32(n))
}

// Planner decides what a version still needs and starts the machines for it.
type Planner struct {
	V     *vault.Vault
	M     MachinesAPI
	GH    GitHub // may be nil: no dispatch
	Cfg   Config
	Sleep func(time.Duration) // between machine creates; nil = time.Sleep
	Now   func() time.Time
}

// ReparseOpts are the knobs of one call.
type ReparseOpts struct {
	DryRun     bool
	Machines   int  // override the machine cap
	Redispatch bool // forget a previous dispatch of this version
	// ResumeOnly is the daily maintain pass: act only if a run of this version
	// was started, and leave replays the server parsed itself since then.
	ResumeOnly bool
}

// Summary is what /vault/reparse answers.
type Summary struct {
	Version        string         `json:"version"`
	DryRun         bool           `json:"dry_run"`
	InVault        int            `json:"in_vault"`
	AlreadyDone    int            `json:"already_done"`
	Todo           int            `json:"todo"`
	ByKind         map[string]int `json:"by_kind"`
	Skipped        map[string]int `json:"skipped"`
	Machines       int            `json:"machines"`
	Started        []string       `json:"started,omitempty"`
	AlreadyRunning bool           `json:"already_running,omitempty"`
	// Locked: another /vault/reparse call is in the middle of starting
	// machines; this one did nothing.
	Locked     bool   `json:"locked,omitempty"`
	Superseded int    `json:"superseded,omitempty"`
	Reaped     int    `json:"reaped,omitempty"`
	Dispatch   string `json:"dispatch,omitempty"`
	Maintain   string `json:"maintain,omitempty"`
	Attempt    string `json:"attempt,omitempty"`
}

func (p *Planner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func (p *Planner) sleep(d time.Duration) {
	if p.Sleep != nil {
		p.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Todo computes what the current version still needs: stored replays without
// a done marker, minus audit (never parsed) and replays this version's parser
// already rejected.
func (p *Planner) Todo(ctx context.Context) ([]PlanItem, Summary, error) {
	return p.todo(ctx, time.Time{})
}

// parsedOnStore: a replay the server stored after the run began, from a
// request it parsed itself with this version (everything but a backfill or a
// local upload). The daily resume leaves those alone.
func parsedOnStore(m vault.Meta, since time.Time) bool {
	if since.IsZero() || m.Source == "valve-backfill" || m.Source == "local-backfill" {
		return false
	}
	at, err := time.Parse(time.RFC3339, m.StoredAt)
	return err == nil && at.After(since)
}

func (p *Planner) todo(ctx context.Context, freshSince time.Time) (items []PlanItem, s Summary, err error) {
	s = Summary{Version: p.Cfg.Version, ByKind: map[string]int{}, Skipped: map[string]int{}}
	ids, err := p.V.ListIDs(ctx)
	if err != nil {
		return nil, s, err
	}
	done, err := p.V.DoneSet(ctx, p.Cfg.Version)
	if err != nil {
		return nil, s, err
	}
	s.InVault = len(ids)
	var pending []int64
	for _, id := range ids {
		if done[id] {
			s.AlreadyDone++
		} else {
			pending = append(pending, id)
		}
	}
	objs, err := p.V.Objects(ctx, pending)
	if err != nil {
		return nil, s, err
	}
	for _, o := range objs {
		switch {
		case !o.Meta.Kind.Reparsed():
			s.Skipped[string(o.Meta.Kind)]++
		case o.Meta.BadFor(p.Cfg.Version):
			// a parser of this version rejected it; a newer release tries again
			s.Skipped["bad"]++
		case parsedOnStore(o.Meta, freshSince):
			s.Skipped["parsed_on_store"]++
		default:
			items = append(items, PlanItem{ID: o.ID, Kind: o.Meta.Kind})
			s.ByKind[string(o.Meta.Kind)]++
		}
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Kind.ReparseOrder() != b.Kind.ReparseOrder() {
			return a.Kind.ReparseOrder() < b.Kind.ReparseOrder()
		}
		return a.ID < b.ID
	})
	s.Todo = len(items)
	return items, s, nil
}

func verSlug(v string) string { return strings.ReplaceAll(v, ".", "-") }

func (p *Planner) workerRequest(plan Plan, shard int, planKey string) CreateRequest {
	env := map[string]string{
		"VAULT_ROLE":          "worker",
		"REPARSE_VERSION":     p.Cfg.Version,
		"REPARSE_PLAN":        planKey,
		"REPARSE_SHARD":       strconv.Itoa(shard),
		"REPARSE_SHARDS":      strconv.Itoa(plan.Shards),
		"REPARSE_PARALLEL":    strconv.Itoa(p.Cfg.Parallel),
		"REPARSE_MAX_RUNTIME": p.Cfg.MaxRuntime.String(),
	}
	for k, v := range p.Cfg.Passthrough {
		if _, set := env[k]; !set {
			env[k] = v
		}
	}
	return CreateRequest{
		Name:   fmt.Sprintf("reparse-%s-%s-s%d", verSlug(p.Cfg.Version), plan.Attempt, shard),
		Region: p.Cfg.Region,
		Config: MachineConfig{
			Image:       p.Cfg.Image,
			Env:         env,
			Guest:       &Guest{CPUKind: "performance", CPUs: p.Cfg.CPUs, MemoryMB: p.Cfg.MemoryMB},
			Restart:     &Restart{Policy: "no"},
			AutoDestroy: true,
			Metadata: map[string]string{"role": "reparse", "version": p.Cfg.Version, "attempt": plan.Attempt,
				"shard": strconv.Itoa(shard), "shards": strconv.Itoa(plan.Shards)},
		},
	}
}

// maintainRequest is the daily scheduled machine (same image, VAULT_ROLE=maintain).
func (p *Planner) maintainRequest() CreateRequest {
	env := map[string]string{"VAULT_ROLE": "maintain"}
	for k, v := range p.Cfg.Passthrough {
		env[k] = v
	}
	return CreateRequest{
		Name:       "vault-maintain",
		Region:     p.Cfg.Region,
		SkipLaunch: true,
		Config: MachineConfig{
			Image:    p.Cfg.Image,
			Env:      env,
			Guest:    &Guest{CPUKind: "performance", CPUs: 1, MemoryMB: 2048},
			Restart:  &Restart{Policy: "no"},
			Schedule: "daily",
			Metadata: map[string]string{"role": "maintain"},
		},
	}
}

// envDiffers: the machine's env is not what the server would set now. Only the
// names the server manages are compared (VAULT_ROLE and the passthrough list),
// so a value dropped from fly.toml also counts as a change.
func envDiffers(have, want map[string]string) bool {
	managed := append([]string{"VAULT_ROLE"}, passthroughKeys...)
	for _, k := range managed {
		if have[k] != want[k] {
			return true
		}
	}
	return false
}

// EnsureMaintain creates the daily maintain machine if there is none, and
// restores an existing one (found by role or name) to the current image,
// schedule and env — a deploy may have touched them, and an API-created
// machine does not pick up fly.toml [env] changes (RETENTION_APPLY, WEB_URL)
// by itself. Idempotent.
func (p *Planner) EnsureMaintain(ctx context.Context) (string, error) {
	ms, err := p.M.List(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if m.Config.Metadata["role"] != "maintain" && m.Name != "vault-maintain" {
			continue
		}
		want := p.maintainRequest()
		if m.Config.Image == p.Cfg.Image && m.Config.Schedule == "daily" && !envDiffers(m.Config.Env, want.Config.Env) {
			return "present", nil
		}
		if err := p.M.Update(ctx, m.ID, want); err != nil {
			return "", err
		}
		return "image_updated", nil
	}
	if _, err := p.M.Create(ctx, p.maintainRequest()); err != nil {
		return "", err
	}
	return "created", nil
}

// Dispatch is the one GitHub dispatch of a run: the marker is created first
// (conditional), so exactly one caller dispatches; a queued run of the
// workflow already waiting counts as the refresh.
func (p *Planner) Dispatch(ctx context.Context) (string, error) {
	return Dispatch(ctx, p.V, p.GH, p.Cfg.Version)
}

// Dispatch creates the dispatched marker and dispatches once.
func Dispatch(ctx context.Context, v *vault.Vault, gh GitHub, ver string) (string, error) {
	if gh == nil {
		return "no_github", nil
	}
	created, err := v.PutJSONIfAbsent(ctx, vault.DispatchedKey(ver), map[string]string{"at": vault.Now().Format(time.RFC3339)})
	if err != nil {
		return "", err
	}
	if !created {
		return "already_dispatched", nil
	}
	waiting, err := gh.RunsWaiting(ctx)
	if err != nil {
		_ = v.S.Delete(ctx, vault.DispatchedKey(ver))
		return "", err
	}
	if waiting {
		return "skipped_run_queued", nil
	}
	if err := gh.Dispatch(ctx); err != nil {
		_ = v.S.Delete(ctx, vault.DispatchedKey(ver))
		return "", err
	}
	return "dispatched", nil
}

// Reparse starts (or resumes) the run of the current version. Safe to call
// again: it works out what is still missing, does nothing while workers of
// this version are alive, and dispatches the page refresh once all is done.
func (p *Planner) Reparse(ctx context.Context, o ReparseOpts) (Summary, error) {
	var fresh time.Time
	if o.ResumeOnly {
		var first Plan
		ok, err := p.V.GetJSON(ctx, vault.PlanKey(p.Cfg.Version), &first)
		if err != nil {
			return Summary{Version: p.Cfg.Version}, err
		}
		if !ok {
			return Summary{Version: p.Cfg.Version, ByKind: map[string]int{}, Skipped: map[string]int{}}, nil
		}
		fresh, _ = time.Parse(time.RFC3339, first.StartedAt)
	}
	items, s, err := p.todo(ctx, fresh)
	s.DryRun = o.DryRun
	if err != nil {
		return s, err
	}
	if p.Cfg.Image == "" && !o.DryRun {
		return s, fmt.Errorf("FLY_IMAGE_REF is not set: cannot start machines")
	}
	s.Machines = p.Cfg.MachinesFor(len(items), o.Machines)
	if len(items) == 0 {
		s.Machines = 0
	}
	if o.DryRun {
		return s, nil
	}

	// One call at a time past this point: two callers that both saw no live
	// worker would otherwise each start a full set of machines.
	release, locked, err := p.lock(ctx)
	if err != nil {
		return s, fmt.Errorf("lock: %w", err)
	}
	if !locked {
		s.Locked = true
		return s, nil
	}
	defer release()

	machines, err := p.M.List(ctx)
	if err != nil {
		return s, err
	}
	alive := 0
	newest := 0                  // newest attempt among the live workers of this version
	liveShards := map[int]bool{} // shards of that attempt with a live machine
	idle := map[string]string{}  // machine name -> id of this version's machines that are not alive
	for _, m := range machines {
		if m.Config.Metadata["role"] != "reparse" {
			continue
		}
		switch {
		case m.Config.Metadata["version"] != p.Cfg.Version:
			if err := p.M.Delete(ctx, m.ID); err == nil {
				s.Superseded++
			}
		case m.Alive() && !m.Created().IsZero() && p.now().Sub(m.Created()) > p.Cfg.MaxRuntime+10*time.Minute:
			if err := p.M.Delete(ctx, m.ID); err == nil {
				s.Reaped++
			}
		case m.Alive():
			alive++
			if a, aerr := strconv.Atoi(m.Config.Metadata["attempt"]); aerr == nil {
				if a > newest {
					newest, liveShards = a, map[int]bool{}
				}
				if k, kerr := strconv.Atoi(m.Config.Metadata["shard"]); kerr == nil && a == newest {
					liveShards[k] = true
				}
			}
		default:
			idle[m.Name] = m.ID
		}
	}

	if o.Redispatch {
		_ = p.V.S.Delete(ctx, vault.DispatchedKey(p.Cfg.Version))
	}

	if len(items) == 0 {
		// Nothing left: a run that finished but whose dispatch failed gets it now.
		if ok, _ := p.V.Exists(ctx, vault.PlanKey(p.Cfg.Version)); ok {
			if d, derr := p.Dispatch(ctx); derr != nil {
				log.Printf("reparse: dispatch: %v", derr)
				s.Dispatch = "error"
			} else {
				s.Dispatch = d
			}
		}
		return s, nil
	}
	if alive > 0 {
		s.AlreadyRunning = true
		s.Machines = alive
		// A call cut off half-way leaves the later shards of its attempt
		// unstarted: create the ones that still have work and no machine.
		if newest > 0 {
			started, err := p.startMissing(ctx, strconv.Itoa(newest), items, liveShards, idle)
			s.Started = started
			s.Machines += len(started)
			if err != nil {
				return s, err
			}
		}
		return s, nil
	}

	plan := Plan{
		Version:   p.Cfg.Version,
		Attempt:   strconv.FormatInt(p.now().Unix(), 10),
		StartedAt: p.now().Format(time.RFC3339),
		Shards:    s.Machines,
		Items:     items,
	}
	s.Attempt = plan.Attempt
	planKey := vault.AttemptKey(p.Cfg.Version, plan.Attempt)
	if err := p.V.PutJSON(ctx, planKey, plan); err != nil {
		return s, fmt.Errorf("write plan: %w", err)
	}
	if _, err := p.V.PutJSONIfAbsent(ctx, vault.PlanKey(p.Cfg.Version), plan); err != nil {
		return s, fmt.Errorf("write plan.json: %w", err)
	}
	for k := 0; k < plan.Shards; k++ {
		if k > 0 {
			p.sleep(300 * time.Millisecond)
		}
		req := p.workerRequest(plan, k, planKey)
		if id, ok := idle[req.Name]; ok {
			_ = p.M.Delete(ctx, id) // a dead machine of the same name would refuse the create
		}
		if _, err := p.M.Create(ctx, req); err != nil {
			return s, fmt.Errorf("create machine %s: %w", req.Name, err)
		}
		s.Started = append(s.Started, req.Name)
	}
	return s, nil
}

// startMissing creates the machines of an attempt's shards that still have
// replays to do and no live machine.
func (p *Planner) startMissing(ctx context.Context, attempt string, todo []PlanItem, live map[int]bool, idle map[string]string) ([]string, error) {
	var plan Plan
	planKey := vault.AttemptKey(p.Cfg.Version, attempt)
	if ok, err := p.V.GetJSON(ctx, planKey, &plan); err != nil || !ok || plan.Shards < 1 {
		return nil, err
	}
	inPlan := map[int64]bool{}
	for _, it := range plan.Items {
		inPlan[it.ID] = true
	}
	hasWork := map[int]bool{}
	for _, it := range todo {
		if inPlan[it.ID] {
			hasWork[Shard(it.ID, plan.Shards)] = true
		}
	}
	var started []string
	for k := 0; k < plan.Shards; k++ {
		if live[k] || !hasWork[k] {
			continue
		}
		if len(started) > 0 {
			p.sleep(300 * time.Millisecond)
		}
		req := p.workerRequest(plan, k, planKey)
		if id, ok := idle[req.Name]; ok {
			_ = p.M.Delete(ctx, id)
		}
		if _, err := p.M.Create(ctx, req); err != nil {
			return started, fmt.Errorf("create machine %s: %w", req.Name, err)
		}
		started = append(started, req.Name)
	}
	return started, nil
}

// lockTTL bounds how long a crashed caller can keep others out.
const lockTTL = 10 * time.Minute

type runLock struct {
	At string `json:"at"`
}

// lock takes the per-version start lock: a conditional create of
// _vault/runs/<ver>/lock. A lock older than lockTTL is stale (its owner died)
// and is taken over. locked=false means another call holds it.
func (p *Planner) lock(ctx context.Context) (release func(), locked bool, err error) {
	key := vault.LockKey(p.Cfg.Version)
	for try := 0; try < 2; try++ {
		created, err := p.V.PutJSONIfAbsent(ctx, key, runLock{At: p.now().Format(time.RFC3339)})
		if err != nil {
			return nil, false, err
		}
		if created {
			return func() { _ = p.V.S.Delete(context.WithoutCancel(ctx), key) }, true, nil
		}
		var cur runLock
		if ok, err := p.V.GetJSON(ctx, key, &cur); err != nil {
			return nil, false, err
		} else if ok {
			if at, perr := time.Parse(time.RFC3339, cur.At); perr == nil && p.now().Sub(at) < lockTTL {
				return nil, false, nil
			}
		}
		_ = p.V.S.Delete(ctx, key) // stale, or released between the two calls
	}
	return nil, false, nil
}
