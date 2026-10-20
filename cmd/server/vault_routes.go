package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"dota-replay-parser/internal/reparse"
	"dota-replay-parser/internal/valve"
	"dota-replay-parser/internal/vault"
)

const maxBackfillItems = 50

type backfillItem struct {
	MatchID  int64             `json:"match_id"`
	Cluster  int               `json:"cluster"`
	Salt     int64             `json:"salt"`
	Kind     string            `json:"kind"`
	Patch    json.RawMessage   `json:"patch"`
	Accounts []json.RawMessage `json:"accounts"`
}

type backfillReq struct {
	Items       []backfillItem `json:"items"`
	Concurrency int            `json:"concurrency"`
}

// handleVaultBackfill stores replays from Valve into the vault without parsing
// them (skip if present, merge the metadata). Callers send batches of up to 50.
func handleVaultBackfill(w http.ResponseWriter, r *http.Request) {
	if !vaultAuthed(w, r) {
		return
	}
	var req backfillReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil || len(req.Items) == 0 {
		http.Error(w, `{"error":"bad_params"}`, http.StatusBadRequest)
		return
	}
	if len(req.Items) > maxBackfillItems {
		http.Error(w, `{"error":"too_many_items","max":50}`, http.StatusRequestEntityTooLarge)
		return
	}
	conc := req.Concurrency
	if conc < 1 {
		conc = 1
	}
	if conc > 4 {
		conc = 4
	}
	results := make([]valve.ItemResult, len(req.Items))
	f := newFetcher()
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, it := range req.Items {
		kind, ok := vault.ParseKind(it.Kind)
		if !ok || it.MatchID <= 0 || it.Cluster <= 0 || it.Salt <= 0 {
			results[i] = valve.ItemResult{MatchID: it.MatchID, Status: valve.StatusFailed, Error: "bad_item"}
			continue
		}
		var accounts []string
		for _, a := range it.Accounts {
			accounts = append(accounts, rawStrings(a)...)
		}
		meta := vault.Meta{Kind: kind, Patch: firstString(it.Patch), Accounts: accounts, Source: "valve-backfill"}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, it backfillItem, meta vault.Meta) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = f.Store(r.Context(), vlt, "backfill", valve.Item{MatchID: it.MatchID, Cluster: it.Cluster, Salt: it.Salt, Meta: meta})
		}(i, it, meta)
	}
	wg.Wait()
	counts := map[string]int{}
	for _, res := range results {
		counts[res.Status]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "counts": counts})
}

func firstString(raw json.RawMessage) string {
	if v := rawStrings(raw); len(v) > 0 {
		return v[0]
	}
	return ""
}

type reparseReq struct {
	DryRun     bool `json:"dry_run"`
	Machines   int  `json:"machines"`
	Redispatch bool `json:"redispatch"`
	// MaintainOnly only brings the daily maintain machine to this server's
	// image and env, and starts no re-parse (release-sync.sh uses it while
	// REPARSE_AUTO is off).
	MaintainOnly bool `json:"maintain_only"`
}

// handleVaultReparse starts (or resumes) the re-parse of the whole vault for
// this server's parser version on temporary machines, and makes sure the daily
// maintain machine exists. Safe to call again.
func handleVaultReparse(w http.ResponseWriter, r *http.Request) {
	if !vaultAuthed(w, r) {
		return
	}
	var req reparseReq
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, `{"error":"bad_params"}`, http.StatusBadRequest)
			return
		}
	}
	if machines == nil && !req.DryRun {
		http.Error(w, `{"error":"fly_token_missing"}`, http.StatusServiceUnavailable)
		return
	}
	p := newPlanner()
	if req.MaintainOnly {
		sum := reparse.Summary{Version: parserVersion}
		m, merr := p.EnsureMaintain(r.Context())
		if merr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "maintain_failed", "detail": merr.Error()})
			return
		}
		sum.Maintain = m
		writeJSON(w, http.StatusOK, sum)
		return
	}
	sum, err := p.Reparse(r.Context(), reparse.ReparseOpts{DryRun: req.DryRun, Machines: req.Machines, Redispatch: req.Redispatch})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "reparse_failed", "detail": err.Error(), "summary": sum})
		return
	}
	if !req.DryRun {
		if m, merr := p.EnsureMaintain(r.Context()); merr != nil {
			sum.Maintain = "error: " + merr.Error()
		} else {
			sum.Maintain = m
		}
	}
	writeJSON(w, http.StatusOK, sum)
}

var (
	statusMu    sync.Mutex
	statusCache reparse.Status
	statusAt    time.Time
)

// handleVaultStatus: counts and bytes per kind, the current run, Valve GETs in
// the last 24 hours. Cached for a minute (it HEADs every object).
func handleVaultStatus(w http.ResponseWriter, r *http.Request) {
	if !vaultAuthed(w, r) {
		return
	}
	statusMu.Lock()
	defer statusMu.Unlock()
	if time.Since(statusAt) > time.Minute || statusCache.Version != parserVersion {
		st, err := newPlanner().BuildStatus(r.Context())
		if err != nil {
			http.Error(w, `{"error":"status_failed"}`, http.StatusBadGateway)
			return
		}
		statusCache, statusAt = st, time.Now()
	}
	writeJSON(w, http.StatusOK, statusCache)
}

func registerVaultRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /vault/backfill", handleVaultBackfill)
	mux.HandleFunc("POST /vault/reparse", handleVaultReparse)
	mux.HandleFunc("GET /vault/status", handleVaultStatus)
}
