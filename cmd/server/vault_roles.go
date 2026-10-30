package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"dota-replay-parser/internal/reparse"
	"dota-replay-parser/internal/retention"
)

// workerParse is the parser handed to the worker: it turns a stored replay
// into the parser's JSON. The replay is copied to local disk first, so a
// network failure reading it is told apart from a replay that does not decode
// (only the second is a verdict on the replay, and a failure to write the
// unpacked file, such as a full disk, is not one either).
func workerParse(ctx context.Context, r io.Reader) ([]byte, error) {
	tmp, err := os.CreateTemp("", "replay-vault-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, cerr := io.Copy(tmp, r)
	if cerr == nil {
		_, cerr = tmp.Seek(0, io.SeekStart)
	}
	if cerr != nil {
		tmp.Close()
		return nil, fmt.Errorf("read replay: %w", cerr)
	}
	demPath, _, err := writeDem(tmp)
	tmp.Close()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, errDecode) {
			return nil, fmt.Errorf("%w: %v", reparse.ErrParseFailed, err)
		}
		// a full disk or a failing temp file says nothing about the replay
		return nil, fmt.Errorf("unpack replay: %w", err)
	}
	defer os.Remove(demPath)
	out, _, err := runParser(ctx, demPath)
	if err != nil {
		if parseExitFailure(ctx, err) {
			return nil, fmt.Errorf("%w: %v", reparse.ErrParseFailed, err)
		}
		return nil, err
	}
	return out, nil
}

// startRole: the role of a machine started as `./server --role=<r>` (the
// Machines API config sets it in init.cmd), else REPLAY_ROLE, else the 4.15.1
// name VAULT_ROLE — which Fly drops from a machine's config env.
func startRole(args []string, getenv func(string) string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "--role=") {
			return strings.TrimPrefix(a, "--role=")
		}
	}
	if r := getenv("REPLAY_ROLE"); r != "" {
		return r
	}
	return getenv("VAULT_ROLE")
}

// runRole is a machine started with --role: the same image as the web
// server, doing one job and exiting. It returns the process exit code.
func runRole(role string) int {
	initVault()
	if vlt == nil {
		log.Printf("role %s: the vault is not configured", role)
		return 1
	}
	ctx := context.Background()
	switch role {
	case "worker":
		w, cfg, err := reparse.WorkerFromEnv(os.Getenv, vlt, workerParse, parserVersion)
		if err != nil {
			log.Printf("role worker: %v", err)
			return 1
		}
		if err := reparse.RunWorker(ctx, w, cfg.MaxRuntime, os.Exit); err != nil {
			return 1
		}
		return 0
	case "maintain":
		if err := runMaintain(ctx); err != nil {
			log.Printf("role maintain: %v", err)
			return 1
		}
		return 0
	}
	log.Printf("unknown role %q", role)
	return 1
}

// runMaintain is the daily pass: (a) resume the current version's run when it
// was started and some replays still lack a marker (or its dispatch failed);
// (b) retention — a dry run unless RETENTION_APPLY=1.
func runMaintain(ctx context.Context) error {
	var errs []error
	p := newPlanner()
	switch {
	case machines == nil:
		log.Printf("maintain: VAULT_FLY_TOKEN missing, resume skipped")
	default:
		sum, err := p.Reparse(ctx, reparse.ReparseOpts{ResumeOnly: true})
		if err != nil {
			errs = append(errs, fmt.Errorf("resume: %w", err))
		}
		log.Printf("maintain: resume todo=%d machines=%d running=%v dispatch=%q", sum.Todo, sum.Machines, sum.AlreadyRunning, sum.Dispatch)
	}

	pct, _ := strconv.Atoi(os.Getenv("RETENTION_MAX_PERCENT"))
	rep, err := retention.Run(ctx, vlt, retention.Config{
		Apply:      os.Getenv("RETENTION_APPLY") == "1",
		MaxPercent: pct,
		WebURL:     vaultCfg.WebURL,
		Token:      vaultCfg.ServiceToken,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
		// the web's games parsed ahead and unopened for 7 days (dry-run until the founder sets it)
		PrefetchApply: os.Getenv("PREFETCH_PURGE_APPLY") == "1",
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("retention: %w", err))
	}
	log.Printf("maintain: retention apply=%v pool=%d candidates=%d deleted=%d unknown_patch=%d aborted=%q error=%q",
		rep.Apply, rep.Pool, rep.Candidates, rep.Deleted, rep.UnknownPatch, rep.Aborted, rep.Error)
	log.Printf("maintain: prefetch purge apply=%v listed=%d candidates=%d deleted=%d error=%q",
		rep.Prefetch.Apply, rep.Prefetch.Listed, rep.Prefetch.Candidates, rep.Prefetch.Deleted, rep.Prefetch.Error)
	return errors.Join(errs...)
}
