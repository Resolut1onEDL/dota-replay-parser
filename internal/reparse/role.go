package reparse

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"dota-replay-parser/internal/vault"
)

// WorkerFromEnv builds the worker of a VAULT_ROLE=worker machine. Secrets come
// from the app's secrets; everything else was set in the machine config. It
// fails fast when something is missing or the image is not the planned version.
func WorkerFromEnv(get func(string) string, v *vault.Vault, parse ParseFunc, parserVersion string) (*Worker, Config, error) {
	cfg := ConfigFromEnv(get, parserVersion)
	want := get("REPARSE_VERSION")
	if want == "" || want != parserVersion {
		return nil, cfg, fmt.Errorf("worker: planned version %q but this image is %q", want, parserVersion)
	}
	if cfg.WebURL == "" || cfg.ServiceToken == "" {
		return nil, cfg, fmt.Errorf("worker: WEB_URL and VAULT_SERVICE_TOKEN missing (secrets not injected?)")
	}
	planKey := get("REPARSE_PLAN")
	if planKey == "" {
		return nil, cfg, fmt.Errorf("worker: REPARSE_PLAN missing")
	}
	shard, err1 := strconv.Atoi(get("REPARSE_SHARD"))
	shards, err2 := strconv.Atoi(get("REPARSE_SHARDS"))
	if err1 != nil || err2 != nil || shards < 1 || shard < 0 || shard >= shards {
		return nil, cfg, fmt.Errorf("worker: bad shard %q of %q", get("REPARSE_SHARD"), get("REPARSE_SHARDS"))
	}
	w := &Worker{
		V: v, Parse: parse, Version: want, PlanKey: planKey, Shard: shard, Shards: shards,
		Parallel: cfg.Parallel, WebURL: cfg.WebURL, Token: cfg.ServiceToken,
	}
	if tok := get("GH_DISPATCH_TOKEN"); tok != "" {
		w.GH = &HTTPGitHub{Token: tok, Repo: get("GH_REPO"), Workflow: get("GH_WORKFLOW")}
	}
	return w, cfg, nil
}

// RunWorker runs a worker with the hard deadline REPARSE_MAX_RUNTIME: when it
// passes the process exits, whatever is still running (the maintain pass
// resumes what is missing).
func RunWorker(ctx context.Context, w *Worker, maxRuntime time.Duration, exit func(int)) error {
	if maxRuntime > 0 {
		t := time.AfterFunc(maxRuntime, func() {
			log.Printf("reparse: hard deadline %s reached, exiting", maxRuntime)
			exit(3)
		})
		defer t.Stop()
	}
	st, err := w.Run(ctx)
	log.Printf("reparse: worker done mine=%d delivered=%d bad=%d failed=%d gone=%d skipped=%d valve_requests=0 err=%v",
		st.Mine, st.Delivered, st.Bad, st.Failed, st.Gone, st.Skipped, err)
	return err
}
