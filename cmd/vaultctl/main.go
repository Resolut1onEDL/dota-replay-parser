// vaultctl is the local tool for the replay vault. It reads the bucket
// credentials from REPLAY_VAULT_* (the same names Fly sets, with that prefix),
// from the environment or from ~/Projects/ResoAI-web/.env.local, and never
// prints them.
//
//	vaultctl [--env FILE] upload --manifest FILE.csv [--concurrency 4] [--dry-run]
//	vaultctl [--env FILE] status [--version X.Y.Z]
//	vaultctl [--env FILE] purge-student --account STEAM32 [--apply]
//	vaultctl [--env FILE] probe
//
// The manifest is a CSV with the columns id,kind,patch,accounts,path
// (accounts separated by ";").
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dota-replay-parser/internal/reparse"
	"dota-replay-parser/internal/vault"
)

func main() {
	args := os.Args[1:]
	envFile := ""
	if len(args) >= 2 && args[0] == "--env" {
		envFile, args = args[1], args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: vaultctl [--env FILE] upload|status|purge-student|probe ...")
		os.Exit(2)
	}
	if envFile == "" {
		home, _ := os.UserHomeDir()
		envFile = filepath.Join(home, "Projects", "ResoAI-web", ".env.local")
	}
	file := readEnvFile(envFile)
	cfg := vault.ConfigFromPrefix(func(k string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return file[k]
	}, "REPLAY_VAULT_")
	if !cfg.Enabled() {
		fmt.Fprintln(os.Stderr, "vaultctl: REPLAY_VAULT_BUCKET_NAME / _AWS_ACCESS_KEY_ID / _AWS_SECRET_ACCESS_KEY are not set (env or "+envFile+")")
		os.Exit(1)
	}
	store, err := vault.NewS3Store(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaultctl:", err)
		os.Exit(1)
	}
	if err := run(context.Background(), args, os.Stdout, vault.New(store)); err != nil {
		fmt.Fprintln(os.Stderr, "vaultctl:", err)
		os.Exit(1)
	}
}

// readEnvFile reads KEY=VALUE lines (optionally quoted) and returns only the
// REPLAY_VAULT_ ones; a missing file is not an error.
func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(k, "REPLAY_VAULT_") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func run(ctx context.Context, args []string, out io.Writer, v *vault.Vault) error {
	switch args[0] {
	case "upload":
		return cmdUpload(ctx, args[1:], out, v)
	case "status":
		return cmdStatus(ctx, args[1:], out, v)
	case "purge-student":
		return cmdPurge(ctx, args[1:], out, v)
	case "probe":
		return cmdProbe(ctx, out, v)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// ---- upload ----

type manifestRow struct {
	ID       int64
	Kind     vault.Kind
	Patch    string
	Accounts []string
	Path     string
}

func readManifest(path string) ([]manifestRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var rows []manifestRow
	for i, rec := range recs {
		if len(rec) < 5 {
			return nil, fmt.Errorf("manifest line %d: want id,kind,patch,accounts,path", i+1)
		}
		id, err := strconv.ParseInt(strings.TrimSpace(rec[0]), 10, 64)
		if err != nil {
			if i == 0 {
				continue // header
			}
			return nil, fmt.Errorf("manifest line %d: bad match id %q", i+1, rec[0])
		}
		kind, ok := vault.ParseKind(rec[1])
		if !ok || strings.TrimSpace(rec[1]) == "" {
			return nil, fmt.Errorf("manifest line %d: unknown kind %q", i+1, rec[1])
		}
		rows = append(rows, manifestRow{ID: id, Kind: kind, Patch: vault.CleanPatch(rec[2]),
			Accounts: vault.SplitAccounts(rec[3]), Path: strings.TrimSpace(rec[4])})
	}
	return rows, nil
}

func fileSHA256(path string) (sum string, size int64, head []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, nil, err
	}
	defer f.Close()
	h := sha256.New()
	size, err = io.Copy(h, f)
	if err != nil {
		return "", 0, nil, err
	}
	f.Seek(0, io.SeekStart)
	head = make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	return hex.EncodeToString(h.Sum(nil)), size, head[:n], nil
}

func cmdUpload(ctx context.Context, args []string, out io.Writer, v *vault.Vault) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "CSV: id,kind,patch,accounts,path")
	conc := fs.Int("concurrency", 4, "files at once")
	dry := fs.Bool("dry-run", false, "hash and compare, upload nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifest == "" {
		return errors.New("upload needs --manifest")
	}
	rows, err := readManifest(*manifest)
	if err != nil {
		return err
	}
	if *conc < 1 {
		*conc = 1
	}
	var mu sync.Mutex
	counts := map[string]int{}
	note := func(status string, row manifestRow, detail string) {
		mu.Lock()
		defer mu.Unlock()
		counts[status]++
		fmt.Fprintf(out, "%-9s %d %s %s\n", status, row.ID, row.Kind, detail)
	}
	sem := make(chan struct{}, *conc)
	var wg sync.WaitGroup
	for _, row := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func(row manifestRow) {
			defer wg.Done()
			defer func() { <-sem }()
			status, detail := uploadOne(ctx, v, row, *dry)
			note(status, row, detail)
		}(row)
	}
	wg.Wait()
	fmt.Fprintf(out, "done: %s\n", summarize(counts))
	if counts["failed"] > 0 || counts["conflict"] > 0 {
		return fmt.Errorf("%d failed, %d conflicting", counts["failed"], counts["conflict"])
	}
	return nil
}

func summarize(c map[string]int) string {
	var parts []string
	for k, n := range c {
		parts = append(parts, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// uploadOne: missing file -> "missing"; stored with the same sha256 (or none
// recorded) -> "present" (metadata merged); stored with another sha256 ->
// "conflict" (never overwritten); otherwise "uploaded".
func uploadOne(ctx context.Context, v *vault.Vault, row manifestRow, dry bool) (string, string) {
	sum, size, head, err := fileSHA256(row.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing", row.Path
		}
		return "failed", err.Error()
	}
	meta := vault.Meta{Kind: row.Kind, Patch: row.Patch, Accounts: row.Accounts, Source: "local-backfill",
		Container: vault.SniffContainer(head), SHA256: sum}
	o, found, err := v.Find(ctx, row.ID)
	if err != nil {
		return "failed", err.Error()
	}
	if found {
		if o.Meta.SHA256 != "" && o.Meta.SHA256 != sum {
			return "conflict", fmt.Sprintf("stored sha256 %s... differs from %s...", o.Meta.SHA256[:8], sum[:8])
		}
		if !dry {
			if _, err := v.UpdateMeta(ctx, row.ID, meta); err != nil {
				return "failed", err.Error()
			}
		}
		return "present", ""
	}
	if dry {
		return "would-upload", fmt.Sprintf("%d MB", size>>20)
	}
	f, err := os.Open(row.Path)
	if err != nil {
		return "failed", err.Error()
	}
	defer f.Close()
	if _, err := v.PutNew(ctx, row.ID, f, size, meta); err != nil {
		return "failed", err.Error()
	}
	if o, ok, err := v.Find(ctx, row.ID); err != nil || !ok || o.Info.Size != size {
		return "failed", "stored size does not match the file"
	}
	return "uploaded", fmt.Sprintf("%d MB", size>>20)
}

// ---- status ----

func cmdStatus(ctx context.Context, args []string, out io.Writer, v *vault.Vault) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	ver := fs.String("version", "", "parser version whose run progress to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := &reparse.Planner{V: v, Cfg: reparse.Config{Version: *ver}}
	st, err := p.BuildStatus(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}

// ---- purge-student ----

func cmdPurge(ctx context.Context, args []string, out io.Writer, v *vault.Vault) error {
	fs := flag.NewFlagSet("purge-student", flag.ContinueOnError)
	account := fs.String("account", "", "steam32 of the student who left")
	apply := fs.Bool("apply", false, "do it (default: list only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(vault.CleanAccounts([]string{*account})) != 1 {
		return errors.New("purge-student needs --account <steam32 digits>")
	}
	ids, err := v.ListIDs(ctx)
	if err != nil {
		return err
	}
	objs, err := v.Objects(ctx, ids)
	if err != nil {
		return err
	}
	var del, detach int
	for _, o := range objs {
		if o.Meta.Kind != vault.KindStudent {
			continue
		}
		has := false
		var rest []string
		for _, a := range o.Meta.Accounts {
			if a == *account {
				has = true
			} else {
				rest = append(rest, a)
			}
		}
		if !has {
			continue
		}
		if len(rest) == 0 {
			del++
			fmt.Fprintf(out, "delete %d\n", o.ID)
			if *apply {
				if err := v.S.Delete(ctx, vault.ReplayKey(o.ID)); err != nil {
					return err
				}
			}
			continue
		}
		detach++
		fmt.Fprintf(out, "detach %d (still wanted by %s)\n", o.ID, strings.Join(rest, ","))
		if *apply {
			m := o.Meta
			m.Accounts = rest
			if err := v.S.CopyReplace(ctx, vault.ReplayKey(o.ID), m.ToS3()); err != nil {
				return err
			}
		}
	}
	mode := "dry run, nothing changed (add --apply)"
	if *apply {
		mode = "applied"
	}
	fmt.Fprintf(out, "%d to delete, %d to detach: %s\n", del, detach, mode)
	return nil
}

// ---- probe ----

// cmdProbe tries what the design leans on and prints what works: conditional
// create (If-None-Match), in-place metadata replace, and the delete of both.
func cmdProbe(ctx context.Context, out io.Writer, v *vault.Vault) error {
	key := fmt.Sprintf("_vault/probe/%d", time.Now().UnixNano())
	defer v.S.Delete(ctx, key)
	body := func() io.ReadSeeker { return strings.NewReader("probe") }
	report := func(name string, err error) {
		if err != nil {
			fmt.Fprintf(out, "FAIL  %-34s %v\n", name, err)
		} else {
			fmt.Fprintf(out, "ok    %s\n", name)
		}
	}
	report("put (plain)", v.S.Put(ctx, key, body(), 5, vault.PutOpts{Meta: map[string]string{"a": "1"}}))
	err := v.S.Put(ctx, key, body(), 5, vault.PutOpts{IfNoneMatch: true})
	if errors.Is(err, vault.ErrExists) {
		report("conditional create refuses an existing key", nil)
	} else {
		report("conditional create refuses an existing key", fmt.Errorf("got %v (the claim fallback is needed)", err))
	}
	key2 := key + "-new"
	defer v.S.Delete(ctx, key2)
	report("conditional create on a free key", v.S.Put(ctx, key2, body(), 5, vault.PutOpts{IfNoneMatch: true}))
	err = v.S.CopyReplace(ctx, key, map[string]string{"a": "2", "b": "3"})
	if err == nil {
		var info vault.ObjectInfo
		info, err = v.S.Head(ctx, key)
		if err == nil && (info.Meta["a"] != "2" || info.Meta["b"] != "3" || info.Size != 5) {
			err = fmt.Errorf("metadata after replace: %v size %d", info.Meta, info.Size)
		}
	}
	report("metadata replace on the same key", err)
	_, err = v.S.Head(ctx, key+"-none")
	if errors.Is(err, vault.ErrNotFound) {
		report("head of a missing key is not-found", nil)
	} else {
		report("head of a missing key is not-found", fmt.Errorf("got %v", err))
	}
	return nil
}
