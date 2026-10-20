package reparse

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dota-replay-parser/internal/vault"
)

// ---- fakes ----

type fakeMachines struct {
	mu      sync.Mutex
	list    []Machine
	created []CreateRequest
	updated []CreateRequest
	deleted []string
	failAt  int // 1-based create that fails
	// when set, the first Create announces itself on entered and waits for gate
	entered chan struct{}
	gate    chan struct{}
	gated   bool
}

func (f *fakeMachines) Create(_ context.Context, r CreateRequest) (Machine, error) {
	f.mu.Lock()
	first := f.entered != nil && !f.gated
	f.gated = true
	f.mu.Unlock()
	if first {
		f.entered <- struct{}{}
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt > 0 && len(f.created)+1 == f.failAt {
		return Machine{}, errors.New("boom")
	}
	f.created = append(f.created, r)
	m := Machine{ID: fmt.Sprintf("m%d", len(f.created)), Name: r.Name, State: "started", Config: r.Config}
	f.list = append(f.list, m)
	return m, nil
}
func (f *fakeMachines) List(context.Context) ([]Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Machine(nil), f.list...), nil
}
func (f *fakeMachines) Update(_ context.Context, id string, r CreateRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, r)
	return nil
}
func (f *fakeMachines) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeGH struct {
	waiting    bool
	dispatches atomic.Int32
	failNext   atomic.Bool
}

func (g *fakeGH) RunsWaiting(context.Context) (bool, error) { return g.waiting, nil }
func (g *fakeGH) Dispatch(context.Context) error {
	if g.failNext.Swap(false) {
		return errors.New("github down")
	}
	g.dispatches.Add(1)
	return nil
}

func testCfg() Config {
	return Config{Version: "4.15.1", App: "resoai-parse", Image: "registry.fly.io/resoai-parse:dep1", Region: "arn",
		MaxMachines: 8, CPUs: 2, MemoryMB: 4096, Parallel: 2, MaxRuntime: 150 * time.Minute, PerSlot: 25,
		WebURL: "http://web.invalid", ServiceToken: "tok", Passthrough: map[string]string{"WEB_URL": "https://reso-ai.com"}}
}

func put(t *testing.T, v *vault.Vault, id int64, m vault.Meta) {
	t.Helper()
	if m.Kind == "" {
		m.Kind = vault.KindCoach
	}
	if _, err := v.PutNew(context.Background(), id, bytes.NewReader([]byte("PBDEMS2 replay")), 14, m); err != nil {
		t.Fatal(err)
	}
}

func newPlanner(t *testing.T) (*Planner, *vault.Vault, *fakeMachines, *fakeGH) {
	t.Helper()
	v := vault.New(vault.NewMemStore())
	fm, gh := &fakeMachines{}, &fakeGH{}
	return &Planner{V: v, M: fm, GH: gh, Cfg: testCfg(), Sleep: func(time.Duration) {},
		Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }}, v, fm, gh
}

// ---- planning ----

func TestShardIsStableAndBalanced(t *testing.T) {
	const n = 8
	counts := make([]int, n)
	for id := int64(9000000000); id < 9000000000+4000; id++ {
		s := Shard(id, n)
		if s != Shard(id, n) || s < 0 || s >= n {
			t.Fatalf("shard(%d) unstable or out of range: %d", id, s)
		}
		counts[s]++
	}
	for k, c := range counts {
		if c < 350 || c > 650 {
			t.Errorf("shard %d got %d of 4000: unbalanced", k, c)
		}
	}
	if Shard(5, 1) != 0 || Shard(5, 0) != 0 {
		t.Fatal("one shard must be shard 0")
	}
}

func TestMachinesFor(t *testing.T) {
	c := testCfg() // 25 * 2 = 50 replays per machine
	for _, tc := range []struct{ todo, override, want int }{
		{0, 0, 1}, {1, 0, 1}, {50, 0, 1}, {51, 0, 2}, {399, 0, 8}, {5000, 0, 8}, {5000, 3, 3}, {120, 0, 3},
		{5000, 500, 8}, {5000, 9, 8}, {120, 500, 3},
	} {
		if got := c.MachinesFor(tc.todo, tc.override); got != tc.want {
			t.Errorf("MachinesFor(%d,%d) = %d, want %d", tc.todo, tc.override, got, tc.want)
		}
	}
}

func TestTodoSkipsAuditBadAndDone_OrderStudentsFirst(t *testing.T) {
	p, v, _, _ := newPlanner(t)
	ctx := context.Background()
	put(t, v, 1, vault.Meta{Kind: vault.KindCoach})
	put(t, v, 2, vault.Meta{Kind: vault.KindStudent})
	put(t, v, 3, vault.Meta{Kind: vault.KindAudit})
	put(t, v, 4, vault.Meta{Kind: vault.KindLadder})
	put(t, v, 5, vault.Meta{Kind: vault.KindPro})
	put(t, v, 6, vault.Meta{Kind: vault.KindCamp})
	put(t, v, 7, vault.Meta{Kind: vault.KindStudent, Bad: true, BadVer: "4.15.1"})
	put(t, v, 8, vault.Meta{Kind: vault.KindStudent})
	// rejected by an older release, or flagged without a version: a new release tries again
	put(t, v, 9, vault.Meta{Kind: vault.KindStudent, Bad: true, BadVer: "4.15.0"})
	put(t, v, 10, vault.Meta{Kind: vault.KindStudent, Bad: true})
	v.WriteDone(ctx, "4.15.1", 8, vault.Done{OK: true})
	v.WriteDone(ctx, "4.15.0", 2, vault.Done{OK: true}) // an older version's marker does not count
	items, s, err := p.Todo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, fmt.Sprintf("%d:%s", it.ID, it.Kind))
	}
	want := "2:student,9:student,10:student,5:pro,4:ladder,1:coach,6:camp"
	if strings.Join(got, ",") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
	if s.Skipped["audit"] != 1 || s.Skipped["bad"] != 1 || s.AlreadyDone != 1 || s.InVault != 10 {
		t.Fatalf("summary = %+v", s)
	}
}

func fill(t *testing.T, v *vault.Vault, n int) {
	for i := 0; i < n; i++ {
		put(t, v, int64(1000+i), vault.Meta{Kind: vault.KindStudent})
	}
}

func TestReparseStartsShardedMachines(t *testing.T) {
	p, v, fm, _ := newPlanner(t)
	fill(t, v, 130) // ceil(130/50) = 3 machines
	s, err := p.Reparse(context.Background(), ReparseOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Todo != 130 || s.Machines != 3 || len(fm.created) != 3 {
		t.Fatalf("summary %+v, created %d", s, len(fm.created))
	}
	r := fm.created[1]
	if r.Config.Init == nil || strings.Join(r.Config.Init.Cmd, " ") != "/app/server --role=worker" {
		t.Fatalf("init = %+v", r.Config.Init)
	}
	assertNoVaultEnv(t, r.Config.Env)
	if r.Config.Env["REPLAY_ROLE"] != "worker" || r.Config.Env["REPARSE_SHARD"] != "1" || r.Config.Env["REPARSE_SHARDS"] != "3" ||
		r.Config.Env["REPARSE_VERSION"] != "4.15.1" || r.Config.Env["WEB_URL"] != "https://reso-ai.com" {
		t.Fatalf("env = %v", r.Config.Env)
	}
	if !r.Config.AutoDestroy || r.Config.Image != "registry.fly.io/resoai-parse:dep1" || r.Config.Guest.CPUs != 2 ||
		r.Config.Guest.CPUKind != "performance" || r.Config.Metadata["role"] != "reparse" || r.Config.Restart.Policy != "no" {
		t.Fatalf("config = %+v", r.Config)
	}
	if strings.Contains(fmt.Sprint(r.Config.Env), "tok") {
		t.Fatal("service token must not travel in the machine config")
	}
	if !strings.HasPrefix(r.Name, "reparse-4-15-1-") || !strings.HasSuffix(r.Name, "-s1") {
		t.Fatalf("name = %s", r.Name)
	}
	var plan Plan
	if ok, _ := v.GetJSON(context.Background(), r.Config.Env["REPARSE_PLAN"], &plan); !ok || len(plan.Items) != 130 || plan.Shards != 3 {
		t.Fatalf("attempt plan = %+v ok=%v", plan, ok)
	}
	if ok, _ := v.Exists(context.Background(), vault.PlanKey("4.15.1")); !ok {
		t.Fatal("plan.json missing")
	}
}

func TestReparseDryRunTouchesNothing(t *testing.T) {
	p, v, fm, _ := newPlanner(t)
	fill(t, v, 10)
	s, err := p.Reparse(context.Background(), ReparseOpts{DryRun: true})
	if err != nil || !s.DryRun || s.Todo != 10 || s.Machines != 1 || len(fm.created) != 0 || len(fm.deleted) != 0 {
		t.Fatalf("dry run: %+v err=%v created=%d", s, err, len(fm.created))
	}
	if ok, _ := v.Exists(context.Background(), vault.PlanKey("4.15.1")); ok {
		t.Fatal("dry run wrote a plan")
	}
}

func TestReparseResumeRulesAndSupersede(t *testing.T) {
	p, v, fm, _ := newPlanner(t)
	ctx := context.Background()
	fill(t, v, 10)
	// a live worker of this version: do nothing more
	fm.list = []Machine{
		{ID: "live", State: "started", CreatedAt: p.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			Config: MachineConfig{Metadata: map[string]string{"role": "reparse", "version": "4.15.1"}}},
		{ID: "old", State: "started", Config: MachineConfig{Metadata: map[string]string{"role": "reparse", "version": "4.15.0"}}},
		{ID: "other", State: "started", Config: MachineConfig{Metadata: map[string]string{"role": "maintain"}}},
	}
	s, _ := p.Reparse(ctx, ReparseOpts{})
	if !s.AlreadyRunning || len(fm.created) != 0 || s.Superseded != 1 || len(fm.deleted) != 1 || fm.deleted[0] != "old" {
		t.Fatalf("with a live worker: %+v created=%d deleted=%v", s, len(fm.created), fm.deleted)
	}
	// the live worker is older than the hard deadline: reaped, then a new run starts
	fm.list = []Machine{{ID: "stale", State: "started", CreatedAt: p.Now().Add(-4 * time.Hour).Format(time.RFC3339),
		Config: MachineConfig{Metadata: map[string]string{"role": "reparse", "version": "4.15.1"}}}}
	fm.deleted = nil
	s, _ = p.Reparse(ctx, ReparseOpts{})
	if s.Reaped != 1 || fm.deleted[0] != "stale" || len(fm.created) != 1 {
		t.Fatalf("stale worker: %+v created=%d", s, len(fm.created))
	}
	// resume: only replays without a marker are re-planned
	for i := 0; i < 6; i++ {
		v.WriteDone(ctx, "4.15.1", int64(1000+i), vault.Done{OK: true})
	}
	fm.list, fm.created = nil, nil
	s, _ = p.Reparse(ctx, ReparseOpts{})
	if s.Todo != 4 || s.AlreadyDone != 6 || len(fm.created) != 1 {
		t.Fatalf("resume: %+v created=%d", s, len(fm.created))
	}
}

func TestReparseNothingLeftDispatchesOnlyAfterARun(t *testing.T) {
	p, v, fm, gh := newPlanner(t)
	ctx := context.Background()
	// nothing in the vault and no run ever planned: nothing happens
	s, _ := p.Reparse(ctx, ReparseOpts{})
	if s.Todo != 0 || len(fm.created) != 0 || gh.dispatches.Load() != 0 || s.Dispatch != "" {
		t.Fatalf("empty vault: %+v", s)
	}
	fill(t, v, 3)
	p.Reparse(ctx, ReparseOpts{}) // plans a run
	for i := 0; i < 3; i++ {
		v.WriteDone(ctx, "4.15.1", int64(1000+i), vault.Done{OK: true})
	}
	fm.list = nil
	s, _ = p.Reparse(ctx, ReparseOpts{})
	if s.Dispatch != "dispatched" || gh.dispatches.Load() != 1 {
		t.Fatalf("finished run: %+v dispatches=%d", s, gh.dispatches.Load())
	}
	s, _ = p.Reparse(ctx, ReparseOpts{})
	if s.Dispatch != "already_dispatched" || gh.dispatches.Load() != 1 {
		t.Fatalf("second call: %+v", s)
	}
	s, _ = p.Reparse(ctx, ReparseOpts{Redispatch: true})
	if gh.dispatches.Load() != 2 {
		t.Fatalf("redispatch: %+v dispatches=%d", s, gh.dispatches.Load())
	}
}

func TestEnsureMaintainIsIdempotent(t *testing.T) {
	p, _, fm, _ := newPlanner(t)
	ctx := context.Background()
	if r, err := p.EnsureMaintain(ctx); err != nil || r != "created" {
		t.Fatalf("first = %q %v", r, err)
	}
	mc := fm.created[0]
	if mc.Config.Init == nil || strings.Join(mc.Config.Init.Cmd, " ") != "/app/server --role=maintain" {
		t.Fatalf("maintain init = %+v", mc.Config.Init)
	}
	assertNoVaultEnv(t, mc.Config.Env)
	if mc.Config.Schedule != "daily" || mc.Config.Env["REPLAY_ROLE"] != "maintain" || mc.Config.Metadata["role"] != "maintain" ||
		mc.Config.AutoDestroy || mc.Config.Image != p.Cfg.Image {
		t.Fatalf("maintain config = %+v", mc.Config)
	}
	if r, _ := p.EnsureMaintain(ctx); r != "present" || len(fm.created) != 1 {
		t.Fatalf("second = %q created=%d", r, len(fm.created))
	}
	p.Cfg.Image = "registry.fly.io/resoai-parse:dep2"
	if r, _ := p.EnsureMaintain(ctx); r != "image_updated" || len(fm.updated) != 1 || !fm.updated[0].SkipLaunch {
		t.Fatalf("after deploy = %q updated=%d", r, len(fm.updated))
	}
}

// An API-created machine keeps the env it was made with: a changed
// RETENTION_APPLY or WEB_URL has to reach it through EnsureMaintain.
func TestEnsureMaintainFollowsTheEnv(t *testing.T) {
	p, _, fm, _ := newPlanner(t)
	ctx := context.Background()
	p.Cfg.Passthrough = map[string]string{"WEB_URL": "https://reso-ai.com", "RETENTION_APPLY": "1"}
	if r, _ := p.EnsureMaintain(ctx); r != "created" {
		t.Fatalf("first = %q", r)
	}
	if r, _ := p.EnsureMaintain(ctx); r != "present" {
		t.Fatalf("unchanged = %q", r)
	}
	// RETENTION_APPLY switched off in fly.toml
	p.Cfg.Passthrough["RETENTION_APPLY"] = "0"
	if r, _ := p.EnsureMaintain(ctx); r != "image_updated" || len(fm.updated) != 1 || fm.updated[0].Config.Env["RETENTION_APPLY"] != "0" {
		t.Fatalf("env change = %q updated=%v", r, fm.updated)
	}
	// the machine as the API now lists it carries the new env
	fm.list[0].Config.Env = fm.updated[0].Config.Env
	if r, _ := p.EnsureMaintain(ctx); r != "present" {
		t.Fatalf("after the update = %q", r)
	}
	// a value removed from the config is removed from the machine as well
	delete(p.Cfg.Passthrough, "RETENTION_APPLY")
	if r, _ := p.EnsureMaintain(ctx); r != "image_updated" || len(fm.updated) != 2 {
		t.Fatalf("removed env = %q updated=%d", r, len(fm.updated))
	}
	if _, has := fm.updated[1].Config.Env["RETENTION_APPLY"]; has {
		t.Fatalf("removed value still sent: %v", fm.updated[1].Config.Env)
	}
}

func TestHTTPMachinesBodyAuthAndRetry(t *testing.T) {
	var calls atomic.Int32
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fly-token" || r.URL.Path != "/v1/apps/resoai-parse/machines" || r.Method != "POST" {
			t.Errorf("request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"id":"abc","name":"x","state":"created"}`))
	}))
	defer srv.Close()
	h := &HTTPMachines{Base: srv.URL, App: "resoai-parse", Token: "fly-token", RetryDelay: time.Millisecond}
	p, _, _, _ := newPlanner(t)
	req := p.workerRequest(Plan{Attempt: "1", Shards: 8}, 3, "_vault/runs/4.15.1/attempt-1.json")
	m, err := h.Create(context.Background(), req)
	if err != nil || m.ID != "abc" || calls.Load() != 2 {
		t.Fatalf("create: %+v err=%v calls=%d", m, err, calls.Load())
	}
	cfg := gotBody["config"].(map[string]any)
	if gotBody["name"] != "reparse-4-15-1-1-s3" || cfg["auto_destroy"] != true || cfg["image"] != p.Cfg.Image ||
		cfg["guest"].(map[string]any)["memory_mb"].(float64) != 4096 {
		t.Fatalf("body = %v", gotBody)
	}
}

// ---- worker ----

type webCall struct {
	mode, id, kind, auth, ctype string
	body                        []byte
}

func fakeWeb(t *testing.T, handler func(c webCall) (int, string)) (*httptest.Server, *[]webCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []webCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c := webCall{mode: r.URL.Query().Get("mode"), id: r.Header.Get("X-Match-Id"), kind: r.Header.Get("X-Vault-Kind"),
			auth: r.Header.Get("Authorization"), ctype: r.Header.Get("Content-Type"), body: b}
		if r.URL.Path != "/api/internal/parse-result" || r.Method != "POST" {
			t.Errorf("web hit %s %s", r.Method, r.URL.Path)
		}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		code, resp := handler(c)
		w.WriteHeader(code)
		w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const okResp = `{"ok":true,"written":{"row":true,"storage":true},"skipped":null}`

func fakeParse(ctx context.Context, r io.Reader) ([]byte, error) {
	b, _ := io.ReadAll(r)
	if strings.Contains(string(b), "BROKEN") {
		return nil, fmt.Errorf("%w: exit 1", ErrParseFailed)
	}
	return []byte(`{"id":1,"startDateTime":1790000000,"parserVersion":"4.15.1"}`), nil
}

func newWorker(t *testing.T, v *vault.Vault, webURL string, gh GitHub, plan Plan, shard, shards int) *Worker {
	t.Helper()
	key := vault.AttemptKey("4.15.1", plan.Attempt)
	if err := v.PutJSON(context.Background(), key, plan); err != nil {
		t.Fatal(err)
	}
	return &Worker{V: v, GH: gh, Parse: fakeParse, Version: "4.15.1", PlanKey: key, Shard: shard, Shards: shards,
		Parallel: 2, WebURL: webURL, Token: "svc-token", Sleep: func(time.Duration) {}, Backoff: time.Millisecond}
}

func TestWorkerDeliversModesAndWritesMarkers(t *testing.T) {
	srv, calls := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	v := vault.New(vault.NewMemStore())
	ctx := context.Background()
	put(t, v, 10, vault.Meta{Kind: vault.KindLadder})
	put(t, v, 11, vault.Meta{Kind: vault.KindStudent})
	put(t, v, 12, vault.Meta{Kind: vault.KindCoach})
	plan := Plan{Version: "4.15.1", Attempt: "1", Shards: 1, Items: []PlanItem{{10, vault.KindLadder}, {11, vault.KindStudent}, {12, vault.KindCoach}}}
	gh := &fakeGH{}
	w := newWorker(t, v, srv.URL, gh, plan, 0, 1)
	st, err := w.Run(ctx)
	if err != nil || st.Delivered != 3 || st.Dispatch != "dispatched" || gh.dispatches.Load() != 1 {
		t.Fatalf("run: %+v err=%v dispatches=%d", st, err, gh.dispatches.Load())
	}
	modes := map[string]string{}
	for _, c := range *calls {
		modes[c.id] = c.mode
		if c.auth != "Bearer svc-token" || c.ctype != "application/gzip" || c.kind == "" {
			t.Errorf("headers = %+v", c)
		}
		zr, err := gzip.NewReader(bytes.NewReader(c.body))
		if err != nil {
			t.Fatalf("body is not gzip: %v", err)
		}
		b, _ := io.ReadAll(zr)
		if !strings.Contains(string(b), `"parserVersion":"4.15.1"`) {
			t.Errorf("gzip body = %s", b)
		}
	}
	if modes["10"] != "storage" || modes["11"] != "ingest" || modes["12"] != "ingest" {
		t.Fatalf("modes = %v", modes)
	}
	d, ok, _ := v.ReadDone(ctx, "4.15.1", 11)
	if !ok || !d.OK || d.Kind != vault.KindStudent || d.Mode != "ingest" || d.StartTime != 1790000000 {
		t.Fatalf("marker = %+v ok=%v", d, ok)
	}
}

func TestWorkerSkipsDoneAndOtherShards(t *testing.T) {
	srv, calls := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	v := vault.New(vault.NewMemStore())
	var items []PlanItem
	for i := int64(1); i <= 40; i++ {
		put(t, v, i, vault.Meta{Kind: vault.KindStudent})
		items = append(items, PlanItem{i, vault.KindStudent})
	}
	v.WriteDone(context.Background(), "4.15.1", 3, vault.Done{OK: true})
	v.WriteDone(context.Background(), "4.15.1", 4, vault.Done{OK: true})
	w := newWorker(t, v, srv.URL, &fakeGH{}, Plan{Attempt: "1", Shards: 2, Items: items}, 0, 2)
	st, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		var id int64
		fmt.Sscan(c.id, &id)
		if Shard(id, 2) != 0 || id == 3 || id == 4 {
			t.Errorf("worker 0/2 delivered %d", id)
		}
	}
	if st.Dispatch != "" {
		t.Fatalf("dispatched while shard 1 is still open: %q", st.Dispatch)
	}
}

func TestWorkerRetriesThen500LeavesNoMarker(t *testing.T) {
	var n atomic.Int32
	srv, _ := fakeWeb(t, func(c webCall) (int, string) {
		if c.id == "20" {
			return 503, ""
		}
		if n.Add(1) == 1 {
			return 500, ""
		}
		return 200, okResp
	})
	v := vault.New(vault.NewMemStore())
	put(t, v, 20, vault.Meta{Kind: vault.KindStudent})
	put(t, v, 21, vault.Meta{Kind: vault.KindStudent})
	w := newWorker(t, v, srv.URL, &fakeGH{}, Plan{Attempt: "1", Shards: 1, Items: []PlanItem{{20, "student"}, {21, "student"}}}, 0, 1)
	w.Parallel = 1
	st, err := w.Run(context.Background())
	if err == nil || st.Failed != 1 || st.Delivered != 1 {
		t.Fatalf("run: %+v err=%v", st, err)
	}
	if ok, _ := v.Exists(context.Background(), vault.DoneKey("4.15.1", 20)); ok {
		t.Fatal("a delivery that never succeeded must not get a marker")
	}
	if ok, _ := v.Exists(context.Background(), vault.DoneKey("4.15.1", 21)); !ok {
		t.Fatal("21 succeeded after one retry and needs its marker")
	}
	if ok, _ := v.Exists(context.Background(), vault.DispatchedKey("4.15.1")); ok {
		t.Fatal("must not dispatch with a replay missing")
	}
	// the web being down says nothing about replay 20: it is not counted against it
	if ok, _ := v.Exists(context.Background(), vault.FailKey("4.15.1", 20)); ok {
		t.Fatal("a delivery failure counted toward giving up on the replay")
	}
}

func TestWorkerParseFailureMarksBadFinal(t *testing.T) {
	srv, calls := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	v := vault.New(vault.NewMemStore())
	ctx := context.Background()
	v.PutNew(ctx, 30, strings.NewReader("BROKEN replay"), 13, vault.Meta{Kind: vault.KindCoach})
	w := newWorker(t, v, srv.URL, &fakeGH{}, Plan{Attempt: "1", Shards: 1, Items: []PlanItem{{30, "coach"}}}, 0, 1)
	st, err := w.Run(ctx)
	if err != nil || st.Bad != 1 || len(*calls) != 0 {
		t.Fatalf("run: %+v err=%v web calls=%d", st, err, len(*calls))
	}
	o, _, _ := v.Find(ctx, 30)
	d, ok, _ := v.ReadDone(ctx, "4.15.1", 30)
	if !o.Meta.BadFor("4.15.1") || !ok || !d.Final || d.OK {
		t.Fatalf("bad=%+v marker=%+v ok=%v", o.Meta, d, ok)
	}
}

// A release that fixes the parser must get another go at the replays the
// previous release rejected, and a good parse clears the old verdict.
func TestWorkerRetriesWhatAnOlderReleaseRejected(t *testing.T) {
	srv, calls := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	v := vault.New(vault.NewMemStore())
	ctx := context.Background()
	put(t, v, 31, vault.Meta{Kind: vault.KindStudent, Bad: true, BadVer: "4.15.0"})
	p, _, _, _ := newPlanner(t)
	p.V = v
	if items, _, _ := p.Todo(ctx); len(items) != 1 || items[0].ID != 31 {
		t.Fatalf("a replay rejected by 4.15.0 must be planned for 4.15.1: %v", items)
	}
	w := newWorker(t, v, srv.URL, &fakeGH{}, Plan{Attempt: "1", Shards: 1, Items: []PlanItem{{31, "student"}}}, 0, 1)
	if st, err := w.Run(ctx); err != nil || st.Delivered != 1 || len(*calls) != 1 {
		t.Fatalf("run: %+v err=%v", st, err)
	}
	if o, _, _ := v.Find(ctx, 31); o.Meta.Bad || o.Meta.BadVer != "" {
		t.Fatalf("a good parse must clear the verdict: %+v", o.Meta)
	}
}

func TestWorkerAuthFailureAndMissingRouteStopTheMachine(t *testing.T) {
	for _, code := range []int{401, 403, 404} {
		srv, calls := fakeWeb(t, func(webCall) (int, string) { return code, "" })
		v := vault.New(vault.NewMemStore())
		var items []PlanItem
		for i := int64(1); i <= 6; i++ {
			put(t, v, i, vault.Meta{Kind: vault.KindStudent})
			items = append(items, PlanItem{i, "student"})
		}
		w := newWorker(t, v, srv.URL, &fakeGH{}, Plan{Attempt: "1", Shards: 1, Items: items}, 0, 1)
		w.Parallel = 1
		_, err := w.Run(context.Background())
		if err == nil || len(*calls) > 2 {
			t.Errorf("%d: err=%v calls=%d; want a stop after the first refusal", code, err, len(*calls))
		}
		if keys := v.S.(*vault.MemStore).Keys("_vault/done/"); len(keys) != 0 {
			t.Errorf("%d: markers written: %v", code, keys)
		}
	}
}

func TestWorkerPermanentRejectionIsFinal(t *testing.T) {
	srv, _ := fakeWeb(t, func(webCall) (int, string) { return 413, "" })
	v := vault.New(vault.NewMemStore())
	put(t, v, 40, vault.Meta{Kind: vault.KindCoach})
	w := newWorker(t, v, srv.URL, &fakeGH{}, Plan{Attempt: "1", Shards: 1, Items: []PlanItem{{40, "coach"}}}, 0, 1)
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, ok, _ := v.ReadDone(context.Background(), "4.15.1", 40)
	if !ok || !d.Final || d.OK || d.Error != "web_413" {
		t.Fatalf("marker = %+v ok=%v", d, ok)
	}
}

func TestDispatchHappensOnceAcrossWorkersAndSkipsQueuedRun(t *testing.T) {
	srv, _ := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	v := vault.New(vault.NewMemStore())
	var items []PlanItem
	for i := int64(1); i <= 30; i++ {
		put(t, v, i, vault.Meta{Kind: vault.KindStudent})
		items = append(items, PlanItem{i, "student"})
	}
	gh := &fakeGH{}
	plan := Plan{Attempt: "1", Shards: 3, Items: items}
	var wg sync.WaitGroup
	for k := 0; k < 3; k++ {
		w := newWorker(t, v, srv.URL, gh, plan, k, 3)
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(context.Background()) }()
	}
	wg.Wait()
	if gh.dispatches.Load() != 1 {
		t.Fatalf("dispatches = %d, want exactly 1", gh.dispatches.Load())
	}

	// a run already waiting counts as the refresh: no second dispatch
	gh2 := &fakeGH{waiting: true}
	d, err := Dispatch(context.Background(), vault.New(vault.NewMemStore()), gh2, "9.9.9")
	if err != nil || d != "skipped_run_queued" || gh2.dispatches.Load() != 0 {
		t.Fatalf("queued: %q %v", d, err)
	}
	// a failed dispatch frees the marker so the maintain pass can retry
	gh3 := &fakeGH{}
	gh3.failNext.Store(true)
	v3 := vault.New(vault.NewMemStore())
	if _, err := Dispatch(context.Background(), v3, gh3, "9.9.9"); err == nil {
		t.Fatal("dispatch error must surface")
	}
	if d, err := Dispatch(context.Background(), v3, gh3, "9.9.9"); err != nil || d != "dispatched" {
		t.Fatalf("retry after failure: %q %v", d, err)
	}
}

func TestWorkerFromEnvFailsFast(t *testing.T) {
	env := map[string]string{"REPARSE_VERSION": "4.15.1", "REPARSE_PLAN": "_vault/runs/4.15.1/attempt-1.json",
		"REPARSE_SHARD": "2", "REPARSE_SHARDS": "4", "WEB_URL": "https://x", "VAULT_SERVICE_TOKEN": "t", "REPARSE_PARALLEL": "3"}
	get := func(k string) string { return env[k] }
	v := vault.New(vault.NewMemStore())
	w, _, err := WorkerFromEnv(get, v, fakeParse, "4.15.1")
	if err != nil || w.Shard != 2 || w.Shards != 4 || w.Parallel != 3 {
		t.Fatalf("worker = %+v err=%v", w, err)
	}
	if _, _, err := WorkerFromEnv(get, v, fakeParse, "4.15.0"); err == nil {
		t.Fatal("an image of another version must refuse to run")
	}
	delete(env, "VAULT_SERVICE_TOKEN")
	if _, _, err := WorkerFromEnv(get, v, fakeParse, "4.15.1"); err == nil {
		t.Fatal("missing secrets must refuse to run")
	}
	env["VAULT_SERVICE_TOKEN"] = "t"
	env["REPARSE_SHARD"] = "4"
	if _, _, err := WorkerFromEnv(get, v, fakeParse, "4.15.1"); err == nil {
		t.Fatal("shard out of range must refuse to run")
	}
}

func TestRunWorkerHardDeadlineExits(t *testing.T) {
	block := make(chan struct{})
	v := vault.New(vault.NewMemStore())
	put(t, v, 50, vault.Meta{Kind: vault.KindStudent})
	srv, _ := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	w := newWorker(t, v, srv.URL, nil, Plan{Attempt: "1", Shards: 1, Items: []PlanItem{{50, "student"}}}, 0, 1)
	w.Parse = func(ctx context.Context, r io.Reader) ([]byte, error) { <-block; return nil, errors.New("late") }
	exited := make(chan int, 1)
	go RunWorker(context.Background(), w, 50*time.Millisecond, func(code int) { exited <- code })
	select {
	case code := <-exited:
		if code != 3 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hard deadline did not fire")
	}
	close(block)
}

// The worker side has no Valve client: proof by package layout (check (b)).
func TestWorkerPackageDoesNotDependOnValve(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "dota-replay-parser/internal/reparse").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "internal/valve") {
			t.Fatalf("internal/reparse depends on %s", line)
		}
	}
	if !strings.Contains(string(out), "dota-replay-parser/internal/vault") {
		t.Fatalf("sanity: the vault package should be in the deps:\n%s", out)
	}
}

func TestBuildStatus(t *testing.T) {
	p, v, _, _ := newPlanner(t)
	ctx := context.Background()
	fill(t, v, 4)
	put(t, v, 99, vault.Meta{Kind: vault.KindLadder, Bad: true, BadVer: "4.15.1"})
	p.Reparse(ctx, ReparseOpts{})
	v.WriteDone(ctx, "4.15.1", 1000, vault.Done{OK: true})
	v.LogValve(ctx, "server", "backfill", 1, 200)
	st, err := p.BuildStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Objects != 5 || st.Kinds["student"].Count != 4 || st.Kinds["ladder"].Bad != 1 || st.DoneThisVer != 1 ||
		st.Run == nil || st.Run.Items != 4 || st.Run.Done != 1 || st.ValveGets24h["server"] != 1 || st.MachinesAlive != 1 {
		b, _ := json.MarshalIndent(st, "", " ")
		t.Fatalf("status = %s", b)
	}
}

func TestResumeOnlyActsOnlyAfterARunAndSkipsWhatTheServerParsed(t *testing.T) {
	p, v, fm, _ := newPlanner(t)
	ctx := context.Background()
	// stored before the run starts (the planner's clock is 12:00 UTC; left empty, PutNew stamps the wall clock)
	put(t, v, 1, vault.Meta{Kind: vault.KindCoach, StoredAt: "2026-10-20T11:00:00Z"})
	// no run was ever started for this version: the daily pass does nothing
	s, err := p.Reparse(ctx, ReparseOpts{ResumeOnly: true})
	if err != nil || s.Todo != 0 || len(fm.created) != 0 {
		t.Fatalf("no run yet: %+v err=%v created=%d", s, err, len(fm.created))
	}
	// a run is started (plan.json at 12:00) ...
	if _, err := p.Reparse(ctx, ReparseOpts{}); err != nil {
		t.Fatal(err)
	}
	fm.list, fm.created = nil, nil
	// ... then three replays arrive: one parsed by the server itself, a backfill, a local upload
	late := "2026-10-20T13:00:00Z"
	put(t, v, 2, vault.Meta{Kind: vault.KindCoach, Source: "analysis", StoredAt: late})
	put(t, v, 3, vault.Meta{Kind: vault.KindLadder, Source: "valve-backfill", StoredAt: late})
	put(t, v, 4, vault.Meta{Kind: vault.KindStudent, Source: "local-backfill", StoredAt: late})
	s, err = p.Reparse(ctx, ReparseOpts{ResumeOnly: true})
	if err != nil || s.Todo != 3 || s.Skipped["parsed_on_store"] != 1 {
		t.Fatalf("resume: %+v err=%v", s, err)
	}
	// a manual /vault/reparse still takes everything without a marker
	fm.list, fm.created = nil, nil
	if s, _ = p.Reparse(ctx, ReparseOpts{DryRun: true}); s.Todo != 4 {
		t.Fatalf("manual dry run todo = %d, want 4", s.Todo)
	}
}

func TestReparseNeedsTheImageRef(t *testing.T) {
	p, v, fm, _ := newPlanner(t)
	fill(t, v, 2)
	p.Cfg.Image = ""
	if _, err := p.Reparse(context.Background(), ReparseOpts{}); err == nil || len(fm.created) != 0 {
		t.Fatalf("err=%v created=%d; an empty image must not reach the Machines API", err, len(fm.created))
	}
	if _, err := p.Reparse(context.Background(), ReparseOpts{DryRun: true}); err != nil {
		t.Fatalf("dry run needs no image: %v", err)
	}
}

// ---- starting machines: partial starts, concurrent calls ----

func TestReparseCompletesAPartiallyStartedAttempt(t *testing.T) {
	p, v, fm, _ := newPlanner(t)
	ctx := context.Background()
	fill(t, v, 400) // 8 machines
	fm.failAt = 4   // the Machines API gives out after three
	if _, err := p.Reparse(ctx, ReparseOpts{}); err == nil {
		t.Fatal("the cut-off call must report the failure")
	}
	if len(fm.created) != 3 {
		t.Fatalf("created %d, want 3", len(fm.created))
	}
	attempt := fm.created[0].Config.Metadata["attempt"]
	// "safe to retry": the second call creates the missing shards of the same attempt
	fm.failAt = 0
	s, err := p.Reparse(ctx, ReparseOpts{})
	if err != nil || !s.AlreadyRunning || len(s.Started) != 5 || len(fm.created) != 8 || s.Machines != 8 {
		t.Fatalf("second call: %+v err=%v created=%d", s, err, len(fm.created))
	}
	seen := map[string]bool{}
	for _, r := range fm.created {
		if r.Config.Metadata["attempt"] != attempt || r.Config.Env["REPARSE_SHARDS"] != "8" ||
			r.Config.Metadata["shard"] != r.Config.Env["REPARSE_SHARD"] {
			t.Fatalf("machine %s: %v", r.Name, r.Config.Metadata)
		}
		if seen[r.Name] {
			t.Fatalf("shard %s started twice", r.Name)
		}
		seen[r.Name] = true
	}
	// a third call finds every shard alive and starts nothing
	s, _ = p.Reparse(ctx, ReparseOpts{})
	if len(s.Started) != 0 || len(fm.created) != 8 {
		t.Fatalf("third call: %+v created=%d", s, len(fm.created))
	}
}

func TestReparseRestartsAShardThatDiedWhileOthersRun(t *testing.T) {
	p, v, fm, _ := newPlanner(t)
	ctx := context.Background()
	fill(t, v, 100) // 2 machines
	if _, err := p.Reparse(ctx, ReparseOpts{}); err != nil || len(fm.created) != 2 {
		t.Fatal(err, len(fm.created))
	}
	// shard 1 is gone (crashed, auto-destroyed) with its replays unfinished; shard 0 still runs
	fm.list = fm.list[:1]
	s, _ := p.Reparse(ctx, ReparseOpts{})
	if len(s.Started) != 1 || !strings.HasSuffix(s.Started[0], "-s1") || len(fm.created) != 3 {
		t.Fatalf("%+v created=%d", s, len(fm.created))
	}
	// shard 1 has nothing left to do: it is not started again
	for i := 0; i < 100; i++ {
		if Shard(int64(1000+i), 2) == 1 {
			v.WriteDone(ctx, "4.15.1", int64(1000+i), vault.Done{OK: true})
		}
	}
	fm.list = fm.list[:1]
	if s, _ = p.Reparse(ctx, ReparseOpts{}); len(s.Started) != 0 || len(fm.created) != 3 {
		t.Fatalf("finished shard restarted: %+v created=%d", s, len(fm.created))
	}
}

func TestConcurrentReparseCallsStartOneSetOfMachines(t *testing.T) {
	p, v, fm, _ := newPlanner(t)
	fill(t, v, 130) // 3 machines
	fm.entered, fm.gate = make(chan struct{}), make(chan struct{})
	first := make(chan Summary, 1)
	go func() {
		s, _ := p.Reparse(context.Background(), ReparseOpts{})
		first <- s
	}()
	<-fm.entered // call A is inside its first machine create, holding the lock
	// call B (a release-sync, a manual curl) arrives at the same moment
	s, err := p.Reparse(context.Background(), ReparseOpts{})
	if err != nil || !s.Locked || len(s.Started) != 0 {
		t.Fatalf("concurrent call: %+v err=%v", s, err)
	}
	close(fm.gate)
	a := <-first
	if len(a.Started) != 3 || len(fm.created) != 3 {
		t.Fatalf("first call: %+v created=%d", a, len(fm.created))
	}
	if ok, _ := v.Exists(context.Background(), vault.LockKey("4.15.1")); ok {
		t.Fatal("the lock was not released")
	}
	// the lock being gone, the next call is an ordinary one
	if s, _ = p.Reparse(context.Background(), ReparseOpts{}); s.Locked || !s.AlreadyRunning {
		t.Fatalf("after: %+v", s)
	}
}

func TestStaleLockIsTakenOver(t *testing.T) {
	p, v, fm, _ := newPlanner(t)
	ctx := context.Background()
	fill(t, v, 10)
	// a caller that died an hour ago left its lock behind
	v.PutJSON(ctx, vault.LockKey("4.15.1"), runLock{At: p.Now().Add(-time.Hour).Format(time.RFC3339)})
	if s, err := p.Reparse(ctx, ReparseOpts{}); err != nil || s.Locked || len(fm.created) != 1 {
		t.Fatalf("stale lock: %+v err=%v created=%d", s, err, len(fm.created))
	}
	// a fresh one holds
	fm.list, fm.created = nil, nil
	v.PutJSON(ctx, vault.LockKey("4.15.1"), runLock{At: p.Now().Add(-time.Minute).Format(time.RFC3339)})
	if s, _ := p.Reparse(ctx, ReparseOpts{}); !s.Locked || len(fm.created) != 0 {
		t.Fatalf("fresh lock: %+v created=%d", s, len(fm.created))
	}
}

// ---- finishing a run: replays that will never get a marker ----

// a replay in `other` shard of n, so that one worker never owns it
func idsInShards(t *testing.T, n int) (a, b int64) {
	t.Helper()
	a, b = -1, -1
	for id := int64(1); id < 200 && (a < 0 || b < 0); id++ {
		switch Shard(id, n) {
		case 0:
			if a < 0 {
				a = id
			}
		case 1:
			if b < 0 {
				b = id
			}
		}
	}
	return a, b
}

func TestWorkerDispatchesWhenAnotherShardsReplayWasDeletedByRetention(t *testing.T) {
	srv, _ := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	v := vault.New(vault.NewMemStore())
	mine, theirs := idsInShards(t, 2)
	put(t, v, mine, vault.Meta{Kind: vault.KindStudent})
	put(t, v, theirs, vault.Meta{Kind: vault.KindCoach})
	plan := Plan{Attempt: "1", Shards: 2, Items: []PlanItem{{mine, "student"}, {theirs, "coach"}}}
	// retention removes the other shard's replay after the plan was made
	v.S.Delete(context.Background(), vault.ReplayKey(theirs))
	gh := &fakeGH{}
	w := newWorker(t, v, srv.URL, gh, plan, 0, 2)
	st, err := w.Run(context.Background())
	if err != nil || st.Dispatch != "dispatched" || gh.dispatches.Load() != 1 {
		t.Fatalf("run: %+v err=%v dispatches=%d", st, err, gh.dispatches.Load())
	}
}

func TestWorkerCountsAGoneReplayAndStillDispatches(t *testing.T) {
	srv, _ := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	v := vault.New(vault.NewMemStore())
	put(t, v, 60, vault.Meta{Kind: vault.KindStudent})
	put(t, v, 61, vault.Meta{Kind: vault.KindStudent})
	plan := Plan{Attempt: "1", Shards: 1, Items: []PlanItem{{60, "student"}, {61, "student"}}}
	v.S.Delete(context.Background(), vault.ReplayKey(61))
	gh := &fakeGH{}
	w := newWorker(t, v, srv.URL, gh, plan, 0, 1)
	st, err := w.Run(context.Background())
	if err != nil || st.Gone != 1 || st.Delivered != 1 || st.Dispatch != "dispatched" {
		t.Fatalf("run: %+v err=%v", st, err)
	}
	// a replay that is still stored but has no marker keeps the dispatch back
	v2 := vault.New(vault.NewMemStore())
	put(t, v2, 70, vault.Meta{Kind: vault.KindStudent})
	w2 := newWorker(t, v2, srv.URL, &fakeGH{}, Plan{Attempt: "1", Shards: 2, Items: []PlanItem{{70, "student"}}}, 1-Shard(70, 2), 2)
	if st, _ := w2.Run(context.Background()); st.Dispatch != "" {
		t.Fatalf("dispatched with replay 70 still open: %+v", st)
	}
}

func TestPlannerDispatchesAfterARunWhoseLastReplayWasDeleted(t *testing.T) {
	p, v, fm, gh := newPlanner(t)
	ctx := context.Background()
	fill(t, v, 3)
	p.Reparse(ctx, ReparseOpts{}) // plans a run
	v.WriteDone(ctx, "4.15.1", 1000, vault.Done{OK: true})
	v.WriteDone(ctx, "4.15.1", 1001, vault.Done{OK: true})
	v.S.Delete(ctx, vault.ReplayKey(1002)) // retention took the third
	fm.list = nil
	s, _ := p.Reparse(ctx, ReparseOpts{})
	if s.Dispatch != "dispatched" || gh.dispatches.Load() != 1 {
		t.Fatalf("%+v dispatches=%d", s, gh.dispatches.Load())
	}
}

func TestWorkerGivesUpOnAReplayThatKeepsFailingByItself(t *testing.T) {
	srv, _ := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	v := vault.New(vault.NewMemStore())
	ctx := context.Background()
	v.PutNew(ctx, 80, strings.NewReader("KILLED replay"), 13, vault.Meta{Kind: vault.KindStudent}) // its parser keeps dying
	put(t, v, 81, vault.Meta{Kind: vault.KindStudent})
	plan := Plan{Attempt: "1", Shards: 1, Items: []PlanItem{{80, "student"}, {81, "student"}}}
	gh := &fakeGH{}
	parse := func(ctx context.Context, r io.Reader) ([]byte, error) {
		if b, _ := io.ReadAll(r); strings.Contains(string(b), "KILLED") {
			return nil, errors.New("signal: killed") // not a verdict on the replay
		}
		return fakeParse(ctx, strings.NewReader("ok"))
	}
	run := func() (Stats, error) {
		w := newWorker(t, v, srv.URL, gh, plan, 0, 1)
		w.Parse = parse
		return w.Run(ctx)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		st, err := run()
		if err == nil || st.Failed != 1 || st.Dispatch != "" {
			t.Fatalf("attempt %d: %+v err=%v", attempt, st, err)
		}
		if ok, _ := v.Exists(ctx, vault.DoneKey("4.15.1", 80)); ok {
			t.Fatalf("attempt %d: given up too early", attempt)
		}
	}
	// the third attempt closes it, and with it the run: nothing is left to wait for
	st, err := run()
	if err != nil || st.Failed != 1 || st.Dispatch != "dispatched" || gh.dispatches.Load() != 1 {
		t.Fatalf("attempt 3: %+v err=%v dispatches=%d", st, err, gh.dispatches.Load())
	}
	d, ok, _ := v.ReadDone(ctx, "4.15.1", 80)
	if !ok || !d.Final || d.OK || d.Error != "gave_up" {
		t.Fatalf("marker = %+v ok=%v", d, ok)
	}
	if o, _, _ := v.Find(ctx, 80); o.Meta.Bad {
		t.Fatal("a replay lost to its parser dying is not flagged bad")
	}
	// and the status says that it was given up on
	p, _, _, _ := newPlanner(t)
	p.V = v
	if status, _ := p.BuildStatus(ctx); status.FinalFailed["gave_up"] != 1 {
		t.Fatalf("final_failed = %v", status.FinalFailed)
	}
}

func TestWorkerDoesNotSendAParseTheWebWouldRefuse(t *testing.T) {
	srv, calls := fakeWeb(t, func(webCall) (int, string) { return 200, okResp })
	v := vault.New(vault.NewMemStore())
	ctx := context.Background()
	put(t, v, 90, vault.Meta{Kind: vault.KindCoach})
	w := newWorker(t, v, srv.URL, &fakeGH{}, Plan{Attempt: "1", Shards: 1, Items: []PlanItem{{90, "coach"}}}, 0, 1)
	w.MaxBody = 10 // a stand-in for the 4.5 MB Vercel limit
	st, err := w.Run(ctx)
	if err != nil || len(*calls) != 0 || st.Failed != 1 {
		t.Fatalf("run: %+v err=%v web calls=%d", st, err, len(*calls))
	}
	d, ok, _ := v.ReadDone(ctx, "4.15.1", 90)
	if !ok || !d.Final || d.OK || d.Error != "too_large" {
		t.Fatalf("marker = %+v", d)
	}
	p, _, _, _ := newPlanner(t)
	p.V = v
	if status, _ := p.BuildStatus(ctx); status.FinalFailed["too_large"] != 1 || status.DoneThisVer != 1 {
		t.Fatalf("status = %+v", status)
	}
}

// Fly drops machine config env names starting with VAULT_ (20.10, 4.15.1): a
// role machine must not depend on one.
func assertNoVaultEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for k := range env {
		if strings.HasPrefix(k, "VAULT_") {
			t.Fatalf("machine env carries %s, which Fly drops", k)
		}
	}
}

// A maintain machine left by 4.15.1 (env role, no init) is updated in place.
func TestEnsureMaintainUpdatesMissingInit(t *testing.T) {
	p, _, fm, _ := newPlanner(t)
	ctx := context.Background()
	if r, err := p.EnsureMaintain(ctx); err != nil || r != "created" {
		t.Fatalf("first = %q %v", r, err)
	}
	fm.list[0].Config.Init = nil
	if r, _ := p.EnsureMaintain(ctx); r == "present" || len(fm.updated) != 1 || fm.updated[0].Config.Init == nil {
		t.Fatalf("missing init not updated: %q updated=%d", r, len(fm.updated))
	}
}
