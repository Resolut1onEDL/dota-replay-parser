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

// nonEmptyStrings is rawStrings without blank entries: an empty body field
// ("kind":"", "account":"") says nothing and must not wipe a header hint.
func nonEmptyStrings(raw json.RawMessage) []string {
	var out []string
	for _, s := range rawStrings(raw) {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
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
		if v := nonEmptyStrings(fields["kind"]); len(v) > 0 {
			kind = v[0]
		}
		if v := nonEmptyStrings(fields["patch"]); len(v) > 0 {
			patch = v[0]
		}
		if v := append(nonEmptyStrings(fields["account"]), nonEmptyStrings(fields["accounts"])...); len(v) > 0 {
			accounts = v
		}
		if v := nonEmptyStrings(fields["source"]); len(v) > 0 {
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

// demMagicOK: the unpacked file starts like a Dota 2 demo. writeDem has
// already decoded the whole stream (a truncated bzip2/zstd body fails there),
// so this is vault.Verify without decompressing twice.
func demMagicOK(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 7)
	if _, err := io.ReadFull(f, magic); err != nil {
		return false
	}
	return string(magic) == "PBDEMS2"
}

// handleParseValveVault is /parse-valve with the vault on: a stored replay is
// read from the vault (no Valve call); otherwise Valve is asked once, the
// bytes are kept, and only a clean download that really is a demo is stored
// (flagged bad when the parser rejects it). A stored replay that does not
// decode, or that the parser rejects for the first time under this version,
// sends the request to Valve once, and a clean download replaces it. The
// caller holds the parse slot.
func handleParseValveVault(w http.ResponseWriter, r *http.Request, req valveReq, h vaultHints) {
	ctx := r.Context()
	var held error // the parse failure of a stored copy, answered if Valve cannot help
	existed := false

	if obj, found, err := vlt.Find(ctx, req.MatchID); err != nil {
		log.Printf("vault: head %d: %v", req.MatchID, err)
	} else if found {
		existed = true
		if _, err := vlt.UpdateMeta(ctx, req.MatchID, h.meta()); err != nil {
			log.Printf("vault: merge meta %d: %v", req.MatchID, err)
		}
		if !obj.Meta.Corrupt {
			switch outcome, perr := serveFromVault(w, r, req.MatchID, obj.Meta); outcome {
			case vaultServed:
				return
			case vaultParseFailed:
				held = perr
			}
		}
	}

	dl, err := newFetcher().Download(ctx, h.Source, req.MatchID, req.Cluster, req.Salt)
	if err != nil {
		if held != nil {
			respondParseFailed(w, held, "hit")
			return
		}
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
		if held != nil {
			respondParseFailed(w, held, "hit")
			return
		}
		http.Error(w, `{"error":"download_failed"}`, http.StatusBadGateway)
		return
	}
	defer os.Remove(demPath)
	log.Printf("parse-valve: downloaded+unpacked %d MB in %s", size>>20, dl.Elapsed.Round(time.Millisecond))

	out, parseDur, perr := runParser(ctx, demPath)

	// A download that decodes into a demo is kept either way (Valve replays
	// expire). Anything else (an HTML error page served with 200, a stream
	// that is not a demo) is never stored. Only the parser's own verdict flags
	// a stored replay bad; a signal kill or a cancelled request says nothing
	// about the replay.
	state := "stored"
	switch {
	case !demMagicOK(demPath):
		log.Printf("parse-valve: download of %d is not a demo (%d bytes), not stored", req.MatchID, dl.Size)
		state = "not_stored"
	case held != nil && perr != nil:
		// the stored copy and the fresh download fail alike: nothing to replace
		state = "hit"
	default:
		meta := h.meta()
		meta.Container = container
		if parseExitFailure(ctx, perr) {
			meta.Bad, meta.BadVer = true, parserVersion
		}
		sctx, cancel := storeCtx(ctx)
		if held != nil {
			// the stored copy failed where this one parses: it was broken, replace it
			if err := vlt.MarkCorrupt(sctx, req.MatchID); err != nil {
				log.Printf("vault: mark corrupt %d: %v", req.MatchID, err)
			}
		}
		raw.Seek(0, io.SeekStart)
		stored, serr := vlt.PutNew(sctx, req.MatchID, raw, dl.Size, meta)
		cancel()
		if serr != nil {
			log.Printf("vault: store %d: %v", req.MatchID, serr)
			state = "store_failed"
		} else if stored && existed {
			state = "replaced" // PutNew only overwrites an object marked corrupt
		}
	}

	if perr != nil {
		respondParseFailed(w, perr, state)
		return
	}
	respondParsed(w, out, parseDur, dl.Elapsed, state)
}

type vaultOutcome int

const (
	vaultServed      vaultOutcome = iota // the request was answered
	vaultUnusable                        // could not be read or decoded: ask Valve
	vaultParseFailed                     // the parser rejected it for the first time under this version: ask Valve once
)

// serveFromVault parses a stored replay and answers it. Otherwise the caller
// falls back to Valve: vaultUnusable for bytes that cannot be read or decoded
// (flagged corrupt when they do not decode, so the clean download replaces
// them), vaultParseFailed with the parser's error for a replay the parser of
// this version has not yet been seen to reject (flagged bad now, so the next
// request does not go back to Valve again).
func serveFromVault(w http.ResponseWriter, r *http.Request, id int64, have vault.Meta) (vaultOutcome, error) {
	ctx := r.Context()
	start := time.Now()
	rc, _, err := vlt.Open(ctx, id)
	if err != nil {
		log.Printf("vault: open %d: %v", id, err)
		return vaultUnusable, nil
	}
	tmp, err := os.CreateTemp("", "replay-vault-*")
	if err != nil {
		rc.Close()
		return vaultUnusable, nil
	}
	defer os.Remove(tmp.Name())
	_, cerr := io.Copy(tmp, rc)
	rc.Close()
	tmp.Seek(0, io.SeekStart)
	if cerr != nil {
		tmp.Close()
		log.Printf("vault: read %d: %v", id, cerr)
		return vaultUnusable, nil
	}
	demPath, size, err := writeDem(tmp)
	tmp.Close()
	if err != nil {
		log.Printf("vault: stored replay %d does not decode: %v", id, err)
		if errors.Is(err, errDecode) {
			if merr := vlt.MarkCorrupt(context.WithoutCancel(ctx), id); merr != nil {
				log.Printf("vault: mark corrupt %d: %v", id, merr)
			}
		}
		return vaultUnusable, nil
	}
	defer os.Remove(demPath)
	dl := time.Since(start)
	log.Printf("parse-valve: vault hit %d, unpacked %d MB in %s", id, size>>20, dl.Round(time.Millisecond))

	out, parseDur, perr := runParser(ctx, demPath)
	if perr != nil {
		if parseExitFailure(ctx, perr) {
			if have.BadFor(parserVersion) {
				respondParseFailed(w, perr, "hit")
				return vaultServed, nil
			}
			if err := vlt.MarkBad(context.WithoutCancel(ctx), id, parserVersion); err != nil {
				log.Printf("vault: flag bad %d: %v", id, err)
			}
			return vaultParseFailed, perr
		}
		respondParseFailed(w, perr, "hit")
		return vaultServed, nil
	}
	if have.Bad {
		// the parser takes it now: an earlier verdict (an older release) is void
		if err := vlt.ClearBad(context.WithoutCancel(ctx), id); err != nil {
			log.Printf("vault: clear bad %d: %v", id, err)
		}
	}
	respondParsed(w, out, parseDur, dl, "hit")
	return vaultServed, nil
}
