package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"dota-replay-parser/internal/reparse"
	"dota-replay-parser/internal/valve"
	"dota-replay-parser/internal/vault"
)

// The vault is off (nil) unless Fly gave the app a Tigris bucket
// (BUCKET_NAME + AWS_*); then every code path below stays out of the way and
// the server behaves exactly as it always did.
var (
	vlt      *vault.Vault
	vaultCfg reparse.Config
	machines reparse.MachinesAPI // nil without VAULT_FLY_TOKEN
	ghClient reparse.GitHub      // nil without GH_DISPATCH_TOKEN
)

func initVault() {
	cfg := vault.ConfigFromEnv(os.Getenv)
	if !cfg.Enabled() {
		log.Printf("vault: off (no bucket configured)")
		return
	}
	store, err := vault.NewS3Store(cfg)
	if err != nil {
		log.Printf("vault: off (%v)", err)
		return
	}
	vlt = vault.New(store)
	vaultCfg = reparse.ConfigFromEnv(os.Getenv, parserVersion)
	if t := os.Getenv("VAULT_FLY_TOKEN"); t != "" {
		machines = &reparse.HTTPMachines{App: vaultCfg.App, Token: t}
	}
	if t := os.Getenv("GH_DISPATCH_TOKEN"); t != "" {
		ghClient = &reparse.HTTPGitHub{Token: t, Repo: os.Getenv("GH_REPO"), Workflow: os.Getenv("GH_WORKFLOW")}
	}
	log.Printf("vault: on (bucket=%s, fly_api=%v, github=%v)", cfg.Bucket, machines != nil, ghClient != nil)
}

func newPlanner() *reparse.Planner {
	return &reparse.Planner{V: vlt, M: machines, GH: ghClient, Cfg: vaultCfg}
}

func newFetcher() *valve.Fetcher {
	return &valve.Fetcher{Client: downloadClient, Vault: vlt, Role: "server"}
}

// vaultAuthed guards every /vault/* route: PARSE_TOKEN must be set (503 when
// it is not — an open vault admin is never the default), the bearer must match
// (401), and the vault must be on (503).
func vaultAuthed(w http.ResponseWriter, r *http.Request) bool {
	if token == "" {
		http.Error(w, `{"error":"vault_auth_unset"}`, http.StatusServiceUnavailable)
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	if vlt == nil {
		http.Error(w, `{"error":"vault_disabled"}`, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// parserEnv is all the parser child sees: it needs no secret (not the vault
// keys, not PARSE_TOKEN, not the Fly or GitHub tokens), only the basics.
func parserEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "TMPDIR", "HOME", "LANG", "LC_ALL", "TZ", "GOGC", "GOMEMLIMIT", "GOMAXPROCS", "CD_DEBUG_PREHORN"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
