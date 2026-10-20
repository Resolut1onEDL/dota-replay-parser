package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"dota-replay-parser/internal/valve"
	"dota-replay-parser/internal/vault"
)

// vaultHints say why a replay is wanted. They travel in X-Vault-* headers
// (the request body stays what it was) or, equally, as body fields; the body
// wins. No kind means a coach analysis.
type vaultHints struct {
	Kind     vault.Kind
	Patch    string
	Accounts []string
	Source   string
}

var sourceRe = regexp.MustCompile(`[^a-z0-9_-]`)

// rawStrings reads a JSON string, number or array of them.
func rawStrings(raw json.RawMessage) []string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var many []json.RawMessage
	if raw[0] == '[' {
		if json.Unmarshal(raw, &many) != nil {
			return nil
		}
	} else {
		many = []json.RawMessage{raw}
	}
	var out []string
	for _, m := range many {
		var s string
		if json.Unmarshal(m, &s) == nil {
			out = append(out, s)
			continue
		}
		var n json.Number
		if json.Unmarshal(m, &n) == nil {
			out = append(out, n.String())
		}
	}
	return out
}

// parseVaultHints reads headers, then body fields over them. ok=false: the
// kind was given but is not one we know.
func parseVaultHints(r *http.Request, body []byte) (vaultHints, bool) {
	kind := r.Header.Get("X-Vault-Kind")
	patch := r.Header.Get("X-Vault-Patch")
	accounts := vault.SplitAccounts(r.Header.Get("X-Vault-Account"))
	source := r.Header.Get("X-Vault-Source")
	var fields map[string]json.RawMessage
	if json.NewDecoder(bytes.NewReader(body)).Decode(&fields) == nil {
		if v := rawStrings(fields["kind"]); len(v) > 0 {
			kind = v[0]
		}
		if v := rawStrings(fields["patch"]); len(v) > 0 {
			patch = v[0]
		}
		if v := append(rawStrings(fields["account"]), rawStrings(fields["accounts"])...); len(v) > 0 {
			accounts = v
		}
		if v := rawStrings(fields["source"]); len(v) > 0 {
			source = v[0]
		}
	}
	k, ok := vault.ParseKind(kind)
	if !ok {
		return vaultHints{}, false
	}
	source = sourceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(source)), "")
	if len(source) > 32 {
		source = source[:32]
	}
	if source == "" {
		source = "valve"
	}
	return vaultHints{Kind: k, Patch: vault.CleanPatch(patch), Accounts: vault.CleanAccounts(accounts), Source: source}, true
}

func (h vaultHints) meta() vault.Meta {
	return vault.Meta{Kind: h.Kind, Patch: h.Patch, Accounts: h.Accounts, Source: h.Source}
}

func storeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	// a finished download is worth keeping even if the caller hung up
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
}

// parseExitFailure: the parser ran and rejected the replay (it exited with a
// status). A signal kill (out of memory) or a cancelled request says nothing
// about the replay and must not flag it bad.
func parseExitFailure(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() > 0
}

// respondParsed answers like serveParsed does, plus the X-Vault header.
func respondParsed(w http.ResponseWriter, out []byte, parseDur, download time.Duration, vaultState string) {
	w.Header().Set("Content-Type", "application/json")
	if vaultState != "" {
		w.Header().Set("X-Vault", vaultState)
	}
	if download > 0 {
		w.Header().Set("X-Download-Ms", fmt.Sprint(download.Milliseconds()))
	}
	w.Header().Set("X-Parse-Ms", fmt.Sprint(parseDur.Milliseconds()))
	log.Printf("parsed in %s (json %d KB)", parseDur.Round(time.Millisecond), len(out)>>10)
	w.Write(out)
}

func respondParseFailed(w http.ResponseWriter, err error, vaultState string) {
	log.Printf("parser failed: %v", err)
	if vaultState != "" {
		w.Header().Set("X-Vault", vaultState)
	}
	http.Error(w, `{"error":"parse_failed"}`, http.StatusUnprocessableEntity)
}

// handleParseValveVault is /parse-valve with the vault on: a stored replay is
// read from the vault (no Valve call); otherwise Valve is asked once, the
// bytes are kept, and only a clean download that parses (or a clean download
// the parser rejects, flagged bad) is stored. The caller holds the parse slot.
func handleParseValveVault(w http.ResponseWriter, r *http.Request, req valveReq, h vaultHints) {
	ctx := r.Context()

	if _, found, err := vlt.Find(ctx, req.MatchID); err != nil {
		log.Printf("vault: head %d: %v", req.MatchID, err)
	} else if found {
		if _, err := vlt.UpdateMeta(ctx, req.MatchID, h.meta()); err != nil {
			log.Printf("vault: merge meta %d: %v", req.MatchID, err)
		}
		if served := serveFromVault(w, r, req.MatchID); served {
			return
		}
	}

	dl, err := newFetcher().Download(ctx, h.Source, req.MatchID, req.Cluster, req.Salt)
	if err != nil {
		var se *valve.StatusError
		switch {
		case errors.As(err, &se):
			log.Printf("parse-valve: valve returned %d", se.Status)
			http.Error(w, fmt.Sprintf(`{"error":"replay_unavailable","valve_status":%d}`, se.Status), http.StatusNotFound)
		case errors.Is(err, valve.ErrTransport):
			log.Printf("parse-valve: %v", err)
			http.Error(w, `{"error":"valve_unreachable"}`, http.StatusBadGateway)
		default:
			log.Printf("parse-valve: %v", err)
			http.Error(w, `{"error":"download_failed"}`, http.StatusBadGateway)
		}
		return
	}
	defer os.Remove(dl.Path)

	raw, err := os.Open(dl.Path)
	if err != nil {
		http.Error(w, `{"error":"download_failed"}`, http.StatusBadGateway)
		return
	}
	defer raw.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(raw, head)
	container := vault.SniffContainer(head[:n])
	raw.Seek(0, io.SeekStart)

	demPath, size, err := writeDem(raw)
	if err != nil {
		log.Printf("parse-valve: undecodable download: %v", err)
		http.Error(w, `{"error":"download_failed"}`, http.StatusBadGateway)
		return
	}
	defer os.Remove(demPath)
	log.Printf("parse-valve: downloaded+unpacked %d MB in %s", size>>20, dl.Elapsed.Round(time.Millisecond))

	out, parseDur, perr := runParser(ctx, demPath)

	// The download is clean, so it is kept either way (Valve replays expire).
	// Only the parser's own verdict flags it bad; a signal kill or a cancelled
	// request says nothing about the replay.
	meta := h.meta()
	meta.Container = container
	meta.Bad = parseExitFailure(ctx, perr)
	state := "stored"
	sctx, cancel := storeCtx(ctx)
	raw.Seek(0, io.SeekStart)
	_, serr := vlt.PutNew(sctx, req.MatchID, raw, dl.Size, meta)
	cancel()
	if serr != nil {
		log.Printf("vault: store %d: %v", req.MatchID, serr)
		state = "store_failed"
	}

	if perr != nil {
		respondParseFailed(w, perr, state)
		return
	}
	respondParsed(w, out, parseDur, dl.Elapsed, state)
}

// serveFromVault parses a stored replay and answers; false means it could not
// be used and the caller should fall back to Valve.
func serveFromVault(w http.ResponseWriter, r *http.Request, id int64) bool {
	ctx := r.Context()
	start := time.Now()
	rc, _, err := vlt.Open(ctx, id)
	if err != nil {
		log.Printf("vault: open %d: %v", id, err)
		return false
	}
	tmp, err := os.CreateTemp("", "replay-vault-*")
	if err != nil {
		rc.Close()
		return false
	}
	defer os.Remove(tmp.Name())
	_, cerr := io.Copy(tmp, rc)
	rc.Close()
	tmp.Seek(0, io.SeekStart)
	if cerr != nil {
		tmp.Close()
		log.Printf("vault: read %d: %v", id, cerr)
		return false
	}
	demPath, size, err := writeDem(tmp)
	tmp.Close()
	if err != nil {
		log.Printf("vault: stored replay %d does not decode: %v", id, err)
		return false
	}
	defer os.Remove(demPath)
	dl := time.Since(start)
	log.Printf("parse-valve: vault hit %d, unpacked %d MB in %s", id, size>>20, dl.Round(time.Millisecond))

	out, parseDur, perr := runParser(ctx, demPath)
	if perr != nil {
		if parseExitFailure(ctx, perr) {
			if _, err := vlt.UpdateMeta(context.WithoutCancel(ctx), id, vault.Meta{Bad: true}); err != nil {
				log.Printf("vault: flag bad %d: %v", id, err)
			}
		}
		respondParseFailed(w, perr, "hit")
		return true
	}
	respondParsed(w, out, parseDur, dl, "hit")
	return true
}
