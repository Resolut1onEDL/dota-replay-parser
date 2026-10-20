package reparse

import (
	"context"
	"sort"
	"strings"
	"time"

	"dota-replay-parser/internal/vault"
)

// KindStats counts one kind in the vault.
type KindStats struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
	Bad   int   `json:"bad,omitempty"`
}

// RunProgress is the newest attempt of the current version.
type RunProgress struct {
	Attempt    string `json:"attempt"`
	StartedAt  string `json:"started_at"`
	Items      int    `json:"items"`
	Done       int    `json:"done"`
	Dispatched bool   `json:"dispatched"`
}

// Status is the answer of GET /vault/status.
type Status struct {
	Version     string               `json:"version"`
	Objects     int                  `json:"objects"`
	Bytes       int64                `json:"bytes"`
	Kinds       map[string]KindStats `json:"kinds"`
	DoneThisVer int                  `json:"done_this_version"`
	// FinalFailed counts the replays whose marker is a final failure (they
	// count as done above but their pages were not refreshed), by error.
	FinalFailed    map[string]int `json:"final_failed,omitempty"`
	Run            *RunProgress   `json:"run,omitempty"`
	ValveGets24h   map[string]int `json:"valve_gets_24h"`
	MachinesAlive  int            `json:"machines_alive"`
	GeneratedAtUTC string         `json:"generated_at"`
}

// BuildStatus counts what the vault holds and how the current run stands.
func (p *Planner) BuildStatus(ctx context.Context) (Status, error) {
	st := Status{Version: p.Cfg.Version, Kinds: map[string]KindStats{}, GeneratedAtUTC: vault.Now().Format(time.RFC3339)}
	ids, err := p.V.ListIDs(ctx)
	if err != nil {
		return st, err
	}
	objs, err := p.V.Objects(ctx, ids)
	if err != nil {
		return st, err
	}
	for _, o := range objs {
		k := st.Kinds[string(o.Meta.Kind)]
		k.Count++
		k.Bytes += o.Info.Size
		if o.Meta.Bad {
			k.Bad++
		}
		st.Kinds[string(o.Meta.Kind)] = k
		st.Objects++
		st.Bytes += o.Info.Size
	}
	done, err := p.V.DoneSet(ctx, p.Cfg.Version)
	if err != nil {
		return st, err
	}
	st.DoneThisVer = len(done)
	if ff, err := p.V.FinalFailures(ctx, p.Cfg.Version); err != nil {
		return st, err
	} else if len(ff) > 0 {
		st.FinalFailed = ff
	}

	var attempts []string
	_ = p.V.S.List(ctx, vault.RunsPrefix(p.Cfg.Version), func(o vault.ObjectInfo) error {
		if strings.Contains(o.Key, "/attempt-") {
			attempts = append(attempts, o.Key)
		}
		return nil
	})
	if len(attempts) > 0 {
		sort.Strings(attempts)
		var plan Plan
		if ok, err := p.V.GetJSON(ctx, attempts[len(attempts)-1], &plan); err == nil && ok {
			rp := &RunProgress{Attempt: plan.Attempt, StartedAt: plan.StartedAt, Items: len(plan.Items)}
			for _, it := range plan.Items {
				if done[it.ID] {
					rp.Done++
				}
			}
			rp.Dispatched, _ = p.V.Exists(ctx, vault.DispatchedKey(p.Cfg.Version))
			st.Run = rp
		}
	}
	st.ValveGets24h, err = p.V.CountValve(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		return st, err
	}
	if p.M != nil {
		if ms, err := p.M.List(ctx); err == nil {
			for _, m := range ms {
				if m.Config.Metadata["role"] == "reparse" && m.Alive() {
					st.MachinesAlive++
				}
			}
		}
	}
	return st, nil
}
