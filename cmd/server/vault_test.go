package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dota-replay-parser/internal/reparse"
	"dota-replay-parser/internal/vault"
)

const zstDemoHex = "28b52ffd2417b90000504244454d53320068656c6c6f2d64656d6f2d626f64794c58d1ad"

// valveFake answers Valve GETs from a table by match id and counts them.
type valveFake struct {
	mu     sync.Mutex
	status map[int64]int    // default 200 with the zstd demo
	body   map[int64][]byte // a different 200 body for a match
	hits   atomic.Int32
}

func (v *valveFake) RoundTrip(r *http.Request) (*http.Response, error) {
	v.hits.Add(1)
	var id int64
	var salt int64
	fmt.Sscanf(r.URL.Path, "/570/%d_%d.dem.bz2", &id, &salt)
	v.mu.Lock()
	code := v.status[id]
	v.mu.Unlock()
	if code == 0 {
		code = 200
	}
	body, _ := hex.DecodeString(zstDemoHex)
	v.mu.Lock()
	if b, ok := v.body[id]; ok {
		body = b
	}
	v.mu.Unlock()
	if code != 200 {
		body = nil
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)), Header: http.Header{}, Request: r}, nil
}

func vaultSetup(t *testing.T) (*vault.MemStore, *valveFake) {
	t.Helper()
	lifecycleSetup(t, 2)
	oldV, oldM, oldG, oldC, oldTok := vlt, machines, ghClient, vaultCfg, token
	mem := vault.NewMemStore()
	vlt = vault.New(mem)
	vaultCfg = reparse.Config{Version: "4.15.1", App: "resoai-parse", Image: "img:1", Region: "arn", MaxMachines: 8, CPUs: 2,
		MemoryMB: 4096, Parallel: 2, MaxRuntime: time.Hour, PerSlot: 25, WebURL: "http://web.invalid", ServiceToken: "svc"}
	t.Cleanup(func() { vlt, machines, ghClient, vaultCfg, token = oldV, oldM, oldG, oldC, oldTok })
	vf := &valveFake{status: map[int64]int{}}
	downloadClient = &http.Client{Transport: vf}
	return mem, vf
}

func writeParser(t *testing.T, script string) {
	t.Helper()
	if err := os.WriteFile(parserBin, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func postValve(body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/parse-valve", strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handleParseValve(w, req)
	return w
}

const body123 = `{"match_id":123,"cluster":1,"salt":456}`

func TestParseValveMissStoresThenHitSkipsValve(t *testing.T) {
	mem, vf := vaultSetup(t)
	w := postValve(body123, map[string]string{"X-Vault-Kind": "student", "X-Vault-Patch": "60", "X-Vault-Account": "1550818411", "X-Vault-Source": "autoparse"})
	if w.Code != 200 || w.Header().Get("X-Vault") != "stored" || w.Body.String() != `{"match_id":123}` {
		t.Fatalf("miss: %d %q X-Vault=%q", w.Code, w.Body.String(), w.Header().Get("X-Vault"))
	}
	o, ok, _ := vlt.Find(context.Background(), 123)
	if !ok || o.Meta.Kind != vault.KindStudent || o.Meta.Patch != "60" || strings.Join(o.Meta.Accounts, ",") != "1550818411" ||
		o.Meta.Source != "autoparse" || o.Meta.Container != vault.ContainerZst || o.Meta.Bad {
		t.Fatalf("stored meta = %+v ok=%v", o.Meta, ok)
	}
	// the bytes are exactly what Valve served
	rc, _, _ := vlt.Open(context.Background(), 123)
	got, _ := io.ReadAll(rc)
	if hex.EncodeToString(got) != zstDemoHex {
		t.Fatalf("stored bytes differ: %x", got)
	}
	if c, _ := vlt.CountValve(context.Background(), time.Now().Add(-time.Hour)); c["server"] != 1 {
		t.Fatalf("valvelog = %v", c)
	}

	// second request: served from the vault, no Valve GET, hints merged
	hitsBefore := vf.hits.Load()
	w = postValve(body123, map[string]string{"X-Vault-Kind": "pro", "X-Vault-Account": "42"})
	if w.Code != 200 || w.Header().Get("X-Vault") != "hit" || vf.hits.Load() != hitsBefore {
		t.Fatalf("hit: %d X-Vault=%q valve hits %d->%d", w.Code, w.Header().Get("X-Vault"), hitsBefore, vf.hits.Load())
	}
	o, _, _ = vlt.Find(context.Background(), 123)
	if o.Meta.Kind != vault.KindStudent || strings.Join(o.Meta.Accounts, ",") != "1550818411,42" {
		t.Fatalf("merged meta = %+v", o.Meta)
	}
	if len(mem.Keys("replays/")) != 1 {
		t.Fatalf("keys = %v", mem.Keys("replays/"))
	}
}

func TestParseValveHintsDefaultsAndValidation(t *testing.T) {
	_, vf := vaultSetup(t)
	postValve(body123, nil)
	if o, _, _ := vlt.Find(context.Background(), 123); o.Meta.Kind != vault.KindCoach || o.Meta.Source != "valve" {
		t.Fatalf("no hints = %+v", o.Meta)
	}
	// body fields win over headers; numbers are accepted as well as strings
	postValve(`{"match_id":124,"cluster":1,"salt":456,"kind":"pro","patch":60,"account":4242}`, map[string]string{"X-Vault-Kind": "student"})
	if o, _, _ := vlt.Find(context.Background(), 124); o.Meta.Kind != vault.KindPro || o.Meta.Patch != "60" || o.Meta.Accounts[0] != "4242" {
		t.Fatalf("body hints = %+v", o.Meta)
	}
	before := vf.hits.Load()
	w := postValve(`{"match_id":125,"cluster":1,"salt":456}`, map[string]string{"X-Vault-Kind": "tournament"})
	if w.Code != 400 || vf.hits.Load() != before {
		t.Fatalf("unknown kind = %d, valve hits %d", w.Code, vf.hits.Load()-before)
	}
}

func TestParseValveParseFailureOnCleanDownloadIsStoredBad(t *testing.T) {
	_, _ = vaultSetup(t)
	writeParser(t, "echo boom >&2; exit 1")
	w := postValve(body123, nil)
	if w.Code != http.StatusUnprocessableEntity || w.Header().Get("X-Vault") != "stored" {
		t.Fatalf("parse failure: %d X-Vault=%q", w.Code, w.Header().Get("X-Vault"))
	}
	if o, ok, _ := vlt.Find(context.Background(), 123); !ok || !o.Meta.Bad {
		t.Fatalf("stored = %+v ok=%v; want bad=1", o.Meta, ok)
	}
}

func TestParseValveKilledParserDoesNotFlagBad(t *testing.T) {
	_, _ = vaultSetup(t)
	writeParser(t, "kill -9 $$")
	w := postValve(body123, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d", w.Code)
	}
	if o, ok, _ := vlt.Find(context.Background(), 123); !ok || o.Meta.Bad {
		t.Fatalf("a signal kill is not a verdict on the replay: %+v ok=%v", o.Meta, ok)
	}
}

func TestParseValveValveErrorsStoreNothing(t *testing.T) {
	_, vf := vaultSetup(t)
	vf.status[123] = 404
	w := postValve(body123, nil)
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"valve_status":404`) {
		t.Fatalf("404: %d %s", w.Code, w.Body.String())
	}
	vf.status[123] = 503
	w = postValve(body123, nil)
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"valve_status":503`) {
		t.Fatalf("503 keeps today's mapping: %d %s", w.Code, w.Body.String())
	}
	if ids, _ := vlt.ListIDs(context.Background()); len(ids) != 0 {
		t.Fatalf("stored %v", ids)
	}
	downloadClient = &http.Client{Transport: lifecycleTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") })}
	if w = postValve(body123, nil); w.Code != 502 || !strings.Contains(w.Body.String(), "valve_unreachable") {
		t.Fatalf("unreachable: %d %s", w.Code, w.Body.String())
	}
}

func TestParseValveStoreFailureNeverFailsTheParse(t *testing.T) {
	mem, _ := vaultSetup(t)
	mem.FailPut = func(key string) error {
		if strings.HasPrefix(key, "replays/") {
			return errors.New("tigris down")
		}
		return nil
	}
	w := postValve(body123, nil)
	if w.Code != 200 || w.Header().Get("X-Vault") != "store_failed" || w.Body.String() != `{"match_id":123}` {
		t.Fatalf("store failure: %d X-Vault=%q body=%q", w.Code, w.Header().Get("X-Vault"), w.Body.String())
	}
}

func TestParseValveUndecodableStoredObjectFallsBackToValve(t *testing.T) {
	mem, vf := vaultSetup(t)
	ctx := context.Background()
	mem.Put(ctx, vault.ReplayKey(123), strings.NewReader("BZh not really bzip2"), 20,
		vault.PutOpts{Meta: vault.Meta{Kind: vault.KindPro, Patch: "60", Accounts: []string{"5"}}.ToS3()})
	w := postValve(body123, map[string]string{"X-Vault-Account": "6"})
	if w.Code != 200 || vf.hits.Load() != 1 || w.Header().Get("X-Vault") != "replaced" {
		t.Fatalf("fallback: %d X-Vault=%q valve hits %d", w.Code, w.Header().Get("X-Vault"), vf.hits.Load())
	}
	// the broken object is healed, not left to fail every later call
	o, _, _ := vlt.Find(ctx, 123)
	rc, _, _ := vlt.Open(ctx, 123)
	got, _ := io.ReadAll(rc)
	if hex.EncodeToString(got) != zstDemoHex || o.Meta.Corrupt || o.Meta.Bad || o.Meta.Container != vault.ContainerZst ||
		o.Meta.Kind != vault.KindPro || o.Meta.Patch != "60" || strings.Join(o.Meta.Accounts, ",") != "5,6" {
		t.Fatalf("healed object = %+v (%d bytes)", o.Meta, len(got))
	}
	w = postValve(body123, nil)
	if w.Code != 200 || w.Header().Get("X-Vault") != "hit" || vf.hits.Load() != 1 {
		t.Fatalf("after healing: %d X-Vault=%q valve hits %d", w.Code, w.Header().Get("X-Vault"), vf.hits.Load())
	}
}

// Valve answering 200 with something that is not a replay (an error page, a
// stream that is not a demo) must never be kept under the replay's key: the
// real replay could not be stored there afterwards.
func TestParseValveNeverStoresADownloadThatIsNotADemo(t *testing.T) {
	_, vf := vaultSetup(t)
	vf.body = map[int64][]byte{123: []byte("<html>502 Bad Gateway</html>")}
	writeParser(t, "echo not a replay >&2; exit 1")
	w := postValve(body123, nil)
	if w.Code != http.StatusUnprocessableEntity || w.Header().Get("X-Vault") != "not_stored" {
		t.Fatalf("html 200: %d X-Vault=%q", w.Code, w.Header().Get("X-Vault"))
	}
	if ids, _ := vlt.ListIDs(context.Background()); len(ids) != 0 {
		t.Fatalf("stored %v", ids)
	}
	// a body that decodes as bzip2/zstd to something other than a demo is no better
	vf.body[124] = []byte("BZh not really bzip2")
	if w = postValve(`{"match_id":124,"cluster":1,"salt":456}`, nil); w.Code != http.StatusBadGateway {
		t.Fatalf("broken bzip2 = %d", w.Code)
	}
	if ids, _ := vlt.ListIDs(context.Background()); len(ids) != 0 {
		t.Fatalf("stored %v", ids)
	}
	// the real replay can still be stored afterwards
	delete(vf.body, 123)
	writeParser(t, `printf '{"match_id":123}'`)
	if w = postValve(body123, nil); w.Code != 200 || w.Header().Get("X-Vault") != "stored" {
		t.Fatalf("real replay: %d X-Vault=%q", w.Code, w.Header().Get("X-Vault"))
	}
}

const parserFailsOnBroken = `grep -q broken "$1" && { echo rejected >&2; exit 1; }; printf '{"match_id":123}'`

// A stored replay the parser rejects, not yet seen to do so under this
// version, may be the vault's fault: Valve is asked once and a clean copy that
// parses replaces it.
func TestParseValveHitThatFailsToParseFallsBackToValveOnce(t *testing.T) {
	mem, vf := vaultSetup(t)
	ctx := context.Background()
	writeParser(t, parserFailsOnBroken)
	mem.Put(ctx, vault.ReplayKey(123), strings.NewReader("PBDEMS2 broken"), 14,
		vault.PutOpts{Meta: vault.Meta{Kind: vault.KindStudent, Accounts: []string{"5"}, Bad: true, BadVer: "0.0.1"}.ToS3()})
	w := postValve(body123, nil)
	if w.Code != 200 || vf.hits.Load() != 1 || w.Header().Get("X-Vault") != "replaced" {
		t.Fatalf("fallback: %d X-Vault=%q valve hits %d", w.Code, w.Header().Get("X-Vault"), vf.hits.Load())
	}
	o, _, _ := vlt.Find(ctx, 123)
	rc, _, _ := vlt.Open(ctx, 123)
	got, _ := io.ReadAll(rc)
	if hex.EncodeToString(got) != zstDemoHex || o.Meta.Bad || o.Meta.Corrupt || o.Meta.Kind != vault.KindStudent {
		t.Fatalf("replaced object = %+v (%d bytes)", o.Meta, len(got))
	}
}

func TestParseValveHitThatKeepsFailingDoesNotGoBackToValve(t *testing.T) {
	mem, vf := vaultSetup(t)
	ctx := context.Background()
	writeParser(t, "exit 1")
	vf.status[123] = 404 // the replay has expired at Valve
	mem.Put(ctx, vault.ReplayKey(123), strings.NewReader("PBDEMS2 x"), 9, vault.PutOpts{Meta: vault.Meta{Kind: vault.KindCoach}.ToS3()})
	w := postValve(body123, nil)
	if w.Code != http.StatusUnprocessableEntity || w.Header().Get("X-Vault") != "hit" || vf.hits.Load() != 1 {
		t.Fatalf("first: %d X-Vault=%q valve hits %d", w.Code, w.Header().Get("X-Vault"), vf.hits.Load())
	}
	if o, _, _ := vlt.Find(ctx, 123); !o.Meta.BadFor(parserVersion) {
		t.Fatalf("the verdict was not recorded: %+v", o.Meta)
	}
	// flagged for this version: answered from the vault, Valve left alone
	w = postValve(body123, nil)
	if w.Code != http.StatusUnprocessableEntity || w.Header().Get("X-Vault") != "hit" || vf.hits.Load() != 1 {
		t.Fatalf("second: %d X-Vault=%q valve hits %d", w.Code, w.Header().Get("X-Vault"), vf.hits.Load())
	}
	// Valve having the replay again but the parser failing on it as well: nothing is replaced
	delete(vf.status, 123)
	mem.CopyReplace(ctx, vault.ReplayKey(123), vault.Meta{Kind: vault.KindCoach}.ToS3())
	w = postValve(body123, nil)
	rc, _, _ := vlt.Open(ctx, 123)
	got, _ := io.ReadAll(rc)
	if w.Code != http.StatusUnprocessableEntity || string(got) != "PBDEMS2 x" {
		t.Fatalf("both fail: %d, stored %q", w.Code, got)
	}
}

func TestParseValveSuccessfulHitVoidsAnOlderVerdict(t *testing.T) {
	mem, _ := vaultSetup(t)
	ctx := context.Background()
	mem.Put(ctx, vault.ReplayKey(123), strings.NewReader("PBDEMS2 fine"), 12,
		vault.PutOpts{Meta: vault.Meta{Kind: vault.KindCoach, Bad: true, BadVer: "0.0.1"}.ToS3()})
	if w := postValve(body123, nil); w.Code != 200 || w.Header().Get("X-Vault") != "hit" {
		t.Fatalf("%d X-Vault=%q", w.Code, w.Header().Get("X-Vault"))
	}
	if o, _, _ := vlt.Find(ctx, 123); o.Meta.Bad {
		t.Fatalf("an old verdict outlived a good parse: %+v", o.Meta)
	}
}

func TestEmptyBodyHintsDoNotWipeHeaderHints(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/parse-valve", nil)
	req.Header.Set("X-Vault-Kind", "student")
	req.Header.Set("X-Vault-Account", "4242")
	req.Header.Set("X-Vault-Patch", "60")
	req.Header.Set("X-Vault-Source", "autoparse")
	h, ok := parseVaultHints(req, []byte(`{"match_id":1,"kind":"","account":"","accounts":[],"patch":"  ","source":""}`))
	if !ok || h.Kind != vault.KindStudent || strings.Join(h.Accounts, ",") != "4242" || h.Patch != "60" || h.Source != "autoparse" {
		t.Fatalf("hints = %+v ok=%v", h, ok)
	}
	// a body that does name something still wins
	h, _ = parseVaultHints(req, []byte(`{"kind":"pro","account":"7"}`))
	if h.Kind != vault.KindPro || strings.Join(h.Accounts, ",") != "7" {
		t.Fatalf("body hints = %+v", h)
	}
}

func TestVaultOffLeavesParseValveAsItWas(t *testing.T) {
	lifecycleSetup(t, 1)
	old := vlt
	vlt = nil
	t.Cleanup(func() { vlt = old })
	vf := &valveFake{status: map[int64]int{}}
	downloadClient = &http.Client{Transport: vf}
	// hint headers and an unknown kind are ignored when the vault is off
	w := postValve(body123, map[string]string{"X-Vault-Kind": "tournament"})
	if w.Code != 200 || w.Header().Get("X-Vault") != "" || w.Body.String() != `{"match_id":123}` {
		t.Fatalf("vault off: %d X-Vault=%q", w.Code, w.Header().Get("X-Vault"))
	}
}

// The vault being off, the parser child gets the environment it always did.
func TestVaultOffLeavesTheParserEnvironmentAlone(t *testing.T) {
	lifecycleSetup(t, 1)
	old := vlt
	vlt = nil
	t.Cleanup(func() { vlt = old })
	t.Setenv("PARSER_TEST_MARKER", "inherited")
	writeParser(t, `printf '{"marker":"%s"}' "$PARSER_TEST_MARKER"`)
	dem := filepath.Join(t.TempDir(), "x.dem")
	os.WriteFile(dem, []byte("PBDEMS2"), 0600)
	out, _, err := runParser(context.Background(), dem)
	if err != nil || string(out) != `{"marker":"inherited"}` {
		t.Fatalf("out=%s err=%v", out, err)
	}
}

func TestParserChildSeesNoSecrets(t *testing.T) {
	vaultSetup(t)
	for k, v := range map[string]string{"AWS_SECRET_ACCESS_KEY": "topsecret-aws", "PARSE_TOKEN": "topsecret-parse",
		"VAULT_FLY_TOKEN": "topsecret-fly", "GH_DISPATCH_TOKEN": "topsecret-gh", "VAULT_SERVICE_TOKEN": "topsecret-svc"} {
		t.Setenv(k, v)
	}
	writeParser(t, `printf '{"env":"'; env | tr '\n' ';'; printf '"}'`)
	dem := filepath.Join(t.TempDir(), "x.dem")
	os.WriteFile(dem, []byte("PBDEMS2"), 0600)
	out, _, err := runParser(context.Background(), dem)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "topsecret") || !strings.Contains(string(out), "PATH=") {
		t.Fatalf("child env = %s", out)
	}
}

// ---- /vault/* routes ----

type routeMachines struct {
	mu      sync.Mutex
	created []reparse.CreateRequest
	list    []reparse.Machine
}

func (f *routeMachines) Create(_ context.Context, r reparse.CreateRequest) (reparse.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, r)
	m := reparse.Machine{ID: fmt.Sprint("m", len(f.created)), Name: r.Name, State: "started", Config: r.Config}
	f.list = append(f.list, m)
	return m, nil
}
func (f *routeMachines) List(context.Context) ([]reparse.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reparse.Machine(nil), f.list...), nil
}
func (f *routeMachines) Update(context.Context, string, reparse.CreateRequest) error { return nil }
func (f *routeMachines) Delete(context.Context, string) error                        { return nil }

func call(h http.HandlerFunc, method, body, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/vault/x", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func TestVaultRoutesAuth(t *testing.T) {
	vaultSetup(t)
	routes := map[string]http.HandlerFunc{"backfill": handleVaultBackfill, "reparse": handleVaultReparse, "status": handleVaultStatus}
	for name, h := range routes {
		token = ""
		if w := call(h, "POST", `{"items":[{}]}`, "anything"); w.Code != 503 || !strings.Contains(w.Body.String(), "vault_auth_unset") {
			t.Errorf("%s with an empty PARSE_TOKEN = %d %s; must be 503", name, w.Code, w.Body.String())
		}
		token = "secret"
		if w := call(h, "POST", `{}`, "wrong"); w.Code != 401 {
			t.Errorf("%s with a bad token = %d", name, w.Code)
		}
		if w := call(h, "POST", `{}`, ""); w.Code != 401 {
			t.Errorf("%s without a token = %d", name, w.Code)
		}
		vlt = nil
		if w := call(h, "POST", `{}`, "secret"); w.Code != 503 || !strings.Contains(w.Body.String(), "vault_disabled") {
			t.Errorf("%s with the vault off = %d %s", name, w.Code, w.Body.String())
		}
		vlt = vault.New(vault.NewMemStore())
	}
}

func TestVaultBackfillStatuses(t *testing.T) {
	_, vf := vaultSetup(t)
	token = "secret"
	vf.status[2] = 404
	vf.status[3] = 503
	vault.New(vlt.S).PutNew(context.Background(), 4, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: vault.KindCoach})
	body := `{"concurrency":9,"items":[
	  {"match_id":1,"cluster":1,"salt":9,"kind":"ladder","patch":60,"accounts":[111,"222"]},
	  {"match_id":2,"cluster":1,"salt":9,"kind":"ladder"},
	  {"match_id":3,"cluster":1,"salt":9,"kind":"ladder"},
	  {"match_id":4,"cluster":1,"salt":9,"kind":"student","accounts":["7"]},
	  {"match_id":5,"cluster":1,"salt":9,"kind":"nonsense"}]}`
	w := call(handleVaultBackfill, "POST", body, "secret")
	if w.Code != 200 {
		t.Fatalf("code %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Results []struct {
			MatchID     int64  `json:"match_id"`
			Status      string `json:"status"`
			ValveStatus int    `json:"valve_status"`
		} `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	want := []string{"stored", "unavailable", "failed", "present", "failed"}
	for i, r := range out.Results {
		if r.Status != want[i] {
			t.Errorf("item %d (%d) = %s, want %s", i, r.MatchID, r.Status, want[i])
		}
	}
	if o, _, _ := vlt.Find(context.Background(), 1); o.Meta.Kind != vault.KindLadder || o.Meta.Patch != "60" ||
		strings.Join(o.Meta.Accounts, ",") != "111,222" || o.Meta.Source != "valve-backfill" {
		t.Fatalf("stored meta = %+v", o.Meta)
	}
	if o, _, _ := vlt.Find(context.Background(), 4); o.Meta.Kind != vault.KindStudent {
		t.Fatalf("present item was not merged: %+v", o.Meta)
	}
	// a URL that is not Valve's (or a China mirror's) file of this match is refused before any download
	bad := `{"items":[{"match_id":6,"cluster":1,"salt":9,"kind":"ladder","url":"http://evil.example/570/6_9.dem.bz2"}]}`
	if w = call(handleVaultBackfill, "POST", bad, "secret"); w.Code != 200 || !strings.Contains(w.Body.String(), `"bad_url"`) {
		t.Fatalf("bad url = %d %s", w.Code, w.Body.String())
	}
	var items []string
	for i := 0; i < 51; i++ {
		items = append(items, `{"match_id":1,"cluster":1,"salt":1}`)
	}
	if w = call(handleVaultBackfill, "POST", `{"items":[`+strings.Join(items, ",")+`]}`, "secret"); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("51 items = %d", w.Code)
	}
}

func TestVaultReparseRouteStartsMachinesAndMaintain(t *testing.T) {
	vaultSetup(t)
	token = "secret"
	for i := int64(1); i <= 3; i++ {
		vlt.PutNew(context.Background(), i, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: vault.KindStudent})
	}
	machines = nil
	if w := call(handleVaultReparse, "POST", `{}`, "secret"); w.Code != 503 {
		t.Fatalf("no Fly token = %d", w.Code)
	}
	w := call(handleVaultReparse, "POST", `{"dry_run":true}`, "secret")
	var sum reparse.Summary
	json.Unmarshal(w.Body.Bytes(), &sum)
	if w.Code != 200 || !sum.DryRun || sum.Todo != 3 {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	fm := &routeMachines{}
	machines = fm
	w = call(handleVaultReparse, "POST", ``, "secret")
	json.Unmarshal(w.Body.Bytes(), &sum)
	if w.Code != 200 || sum.Machines != 1 || len(sum.Started) != 1 || sum.Maintain != "created" || len(fm.created) != 2 {
		t.Fatalf("reparse: %d %s created=%d", w.Code, w.Body.String(), len(fm.created))
	}
	// again: the worker is alive, the maintain machine exists -> nothing new
	w = call(handleVaultReparse, "POST", ``, "secret")
	json.Unmarshal(w.Body.Bytes(), &sum)
	if !sum.AlreadyRunning || len(fm.created) != 2 {
		t.Fatalf("second call: %s created=%d", w.Body.String(), len(fm.created))
	}
}

func TestVaultReparseCapsTheMachineCountAtTheConfiguredMaximum(t *testing.T) {
	vaultSetup(t)
	token = "secret"
	for i := int64(1); i <= 500; i++ {
		vlt.PutNew(context.Background(), i, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: vault.KindStudent})
	}
	fm := &routeMachines{}
	machines = fm
	w := call(handleVaultReparse, "POST", `{"machines":500}`, "secret")
	var sum reparse.Summary
	json.Unmarshal(w.Body.Bytes(), &sum)
	if w.Code != 200 || sum.Machines != vaultCfg.MaxMachines || len(sum.Started) != vaultCfg.MaxMachines {
		t.Fatalf("machines=500 started %d (cap %d): %s", len(sum.Started), vaultCfg.MaxMachines, w.Body.String())
	}
}

func TestVaultReparseMaintainOnlyStartsNoRun(t *testing.T) {
	vaultSetup(t)
	token = "secret"
	for i := int64(1); i <= 3; i++ {
		vlt.PutNew(context.Background(), i, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: vault.KindStudent})
	}
	fm := &routeMachines{}
	machines = fm
	w := call(handleVaultReparse, "POST", `{"maintain_only":true}`, "secret")
	var sum reparse.Summary
	json.Unmarshal(w.Body.Bytes(), &sum)
	if w.Code != 200 || sum.Maintain != "created" || len(fm.created) != 1 || fm.created[0].Config.Metadata["role"] != "maintain" {
		t.Fatalf("maintain only: %d %s created=%d", w.Code, w.Body.String(), len(fm.created))
	}
	if ok, _ := vlt.Exists(context.Background(), vault.PlanKey("4.15.1")); ok {
		t.Fatal("maintain_only planned a run")
	}
	// the maintain machine follows a changed env on the next release
	vaultCfg.Passthrough = map[string]string{"RETENTION_APPLY": "1"}
	fm.list[0].Config.Env = fm.created[0].Config.Env
	w = call(handleVaultReparse, "POST", `{"maintain_only":true}`, "secret")
	json.Unmarshal(w.Body.Bytes(), &sum)
	if sum.Maintain != "image_updated" {
		t.Fatalf("env change: %s", w.Body.String())
	}
	machines = nil
	if w = call(handleVaultReparse, "POST", `{"maintain_only":true}`, "secret"); w.Code != 503 {
		t.Fatalf("no Fly token = %d", w.Code)
	}
}

func TestVaultStatusRoute(t *testing.T) {
	vaultSetup(t)
	token = "secret"
	statusAt = time.Time{}
	vlt.PutNew(context.Background(), 1, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: vault.KindPro})
	w := call(handleVaultStatus, "GET", ``, "secret")
	var st reparse.Status
	json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != 200 || st.Objects != 1 || st.Kinds["pro"].Count != 1 {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
}

// ---- the worker role, end to end ----

func TestWorkerRoleParsesFromTheVaultWithoutTouchingValve(t *testing.T) {
	_, vf := vaultSetup(t)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("mode") == "" || r.Header.Get("Authorization") != "Bearer svc" || len(b) == 0 {
			t.Errorf("bad delivery: %s %v", r.URL, r.Header)
		}
		w.Write([]byte(`{"ok":true,"written":{"row":true,"storage":true},"skipped":null}`))
	}))
	defer web.Close()
	ctx := context.Background()
	zst, _ := hex.DecodeString(zstDemoHex)
	for id, kind := range map[int64]vault.Kind{1: vault.KindStudent, 2: vault.KindLadder} {
		vlt.PutNew(ctx, id, bytes.NewReader(zst), int64(len(zst)), vault.Meta{Kind: kind})
	}
	plan := reparse.Plan{Version: "4.15.1", Attempt: "7", Shards: 1,
		Items: []reparse.PlanItem{{ID: 1, Kind: vault.KindStudent}, {ID: 2, Kind: vault.KindLadder}}}
	vlt.PutJSON(ctx, vault.AttemptKey("4.15.1", "7"), plan)
	env := map[string]string{"REPARSE_VERSION": parserVersion, "REPARSE_PLAN": vault.AttemptKey("4.15.1", "7"),
		"REPARSE_SHARD": "0", "REPARSE_SHARDS": "1", "WEB_URL": web.URL, "VAULT_SERVICE_TOKEN": "svc"}
	w, cfg, err := reparse.WorkerFromEnv(func(k string) string { return env[k] }, vlt, workerParse, parserVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := reparse.RunWorker(ctx, w, cfg.MaxRuntime, func(int) { t.Error("exit called") }); err != nil {
		t.Fatal(err)
	}
	if set, _ := vlt.DoneSet(ctx, parserVersion); len(set) != 2 {
		t.Fatalf("markers = %v", set)
	}
	if vf.hits.Load() != 0 {
		t.Fatalf("the worker made %d Valve requests", vf.hits.Load())
	}
}

func TestWorkerParseMapsRejectionToParseFailed(t *testing.T) {
	lifecycleSetup(t, 1)
	zst, _ := hex.DecodeString(zstDemoHex)
	writeParser(t, "exit 1")
	if _, err := workerParse(context.Background(), bytes.NewReader(zst)); !errors.Is(err, reparse.ErrParseFailed) {
		t.Fatalf("parser exit 1 = %v; want ErrParseFailed", err)
	}
	writeParser(t, "kill -9 $$")
	if _, err := workerParse(context.Background(), bytes.NewReader(zst)); err == nil || errors.Is(err, reparse.ErrParseFailed) {
		t.Fatalf("a signal kill = %v; must not be a verdict on the replay", err)
	}
	if _, err := workerParse(context.Background(), bytes.NewReader([]byte("BZh garbage"))); !errors.Is(err, reparse.ErrParseFailed) {
		t.Fatalf("undecodable replay = %v; want ErrParseFailed", err)
	}
	errR := io.MultiReader(bytes.NewReader(zst[:5]), errReader{})
	if _, err := workerParse(context.Background(), errR); err == nil || errors.Is(err, reparse.ErrParseFailed) {
		t.Fatalf("a failing read = %v; must be retryable, not bad", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("tigris reset") }
