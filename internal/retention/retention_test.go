package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"dota-replay-parser/internal/vault"
)

var now = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// 7.39 (58) and 7.40 (59) are old; 7.41 (60) is current; 7.42 (61) is announced for the future.
const patchJSON = `[
 {"id":58,"name":"7.39","date":"2025-05-22T00:00:00Z"},
 {"id":59,"name":"7.40","date":"2025-12-16T00:00:00Z"},
 {"id":60,"name":"7.41","date":"2026-03-24T00:50:59Z"},
 {"id":61,"name":"7.42","date":"2026-12-01T00:00:00Z"}]`

type env struct {
	v       *vault.Vault
	mem     *vault.MemStore
	od      *httptest.Server
	web     *httptest.Server
	pinsOK  bool
	pins    []int64
	purge   []int64
	purgeOK bool
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{mem: vault.NewMemStore(), pinsOK: true}
	e.v = vault.New(e.mem)
	e.od = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/constants/patch" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(patchJSON))
	}))
	e.web = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/vault/purge" && r.Header.Get("Authorization") == "Bearer svc" && e.purgeOK {
			json.NewEncoder(w).Encode(map[string]any{"prefetch": e.purge})
			return
		}
		if r.URL.Path != "/api/internal/vault/pins" || r.Header.Get("Authorization") != "Bearer svc" || !e.pinsOK {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"pro": e.pins})
	}))
	t.Cleanup(func() { e.od.Close(); e.web.Close() })
	return e
}

func (e *env) cfg(apply bool) Config {
	return Config{Apply: apply, OpenDota: e.od.URL, WebURL: e.web.URL, Token: "svc", Now: func() time.Time { return now }}
}

func (e *env) put(t *testing.T, id int64, kind vault.Kind, patch string) {
	t.Helper()
	if _, err := e.v.PutNew(context.Background(), id, bytes.NewReader([]byte("x")), 1, vault.Meta{Kind: kind, Patch: patch}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) has(id int64) bool {
	ok, _ := e.v.Exists(context.Background(), vault.ReplayKey(id))
	return ok
}

// 20 recent coach games (patch 60) keep the 25 % guard far away from the old ones.
func (e *env) recent(t *testing.T, n int) {
	for i := 0; i < n; i++ {
		e.put(t, int64(5000+i), vault.KindCoach, "60")
	}
}

func TestDryRunReportsButDeletesNothing(t *testing.T) {
	e := setup(t)
	e.recent(t, 20)
	e.put(t, 1, vault.KindCoach, "58") // older than the previous patch (59)
	rep, err := Run(context.Background(), e.v, e.cfg(false))
	if err != nil {
		t.Fatal(err)
	}
	if rep.CurrentPatch != 60 || rep.PreviousPatch != 59 || rep.Candidates != 1 || rep.Deleted != 0 || rep.Apply {
		t.Fatalf("report = %+v", rep)
	}
	if !e.has(1) {
		t.Fatal("dry run deleted a replay")
	}
	if ok, _ := e.v.Exists(context.Background(), vault.RetentionKey("20261020")); !ok {
		t.Fatal("the report object was not written")
	}
}

func TestApplyDeletesOnlyOlderThanPreviousAndOnlyCoachAndPro(t *testing.T) {
	e := setup(t)
	e.recent(t, 40)
	e.pins = []int64{4}
	e.put(t, 1, vault.KindCoach, "58")   // goes
	e.put(t, 2, vault.KindPro, "58")     // goes
	e.put(t, 3, vault.KindCoach, "59")   // previous patch: stays
	e.put(t, 4, vault.KindPro, "58")     // pinned by the web: stays
	e.put(t, 5, vault.KindStudent, "58") // never
	e.put(t, 6, vault.KindLadder, "58")  // never
	e.put(t, 7, vault.KindAudit, "58")   // never
	e.put(t, 8, vault.KindCamp, "58")    // never
	rep, err := Run(context.Background(), e.v, e.cfg(true))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 2 || rep.PinnedPro != 1 {
		t.Fatalf("report = %+v", rep)
	}
	for id, want := range map[int64]bool{1: false, 2: false, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true} {
		if e.has(id) != want {
			t.Errorf("replay %d present=%v, want %v", id, e.has(id), want)
		}
	}
}

func TestNoPinsRouteMeansNoProIsTouched(t *testing.T) {
	e := setup(t)
	e.recent(t, 40)
	e.pinsOK = false
	e.put(t, 1, vault.KindPro, "58")
	e.put(t, 2, vault.KindCoach, "58")
	rep, _ := Run(context.Background(), e.v, e.cfg(true))
	if rep.ProSkipped != 1 || !e.has(1) || e.has(2) {
		t.Fatalf("report = %+v pro=%v coach=%v", rep, e.has(1), e.has(2))
	}
}

func TestGuardAbortsBeyondAQuarter(t *testing.T) {
	e := setup(t)
	e.recent(t, 6)
	for i := 0; i < 4; i++ { // 4 of 10 = 40 %
		e.put(t, int64(10+i), vault.KindCoach, "57")
	}
	rep, err := Run(context.Background(), e.v, e.cfg(true))
	if err != nil || rep.Deleted != 0 || rep.Aborted == "" || rep.Candidates != 4 {
		t.Fatalf("report = %+v err=%v", rep, err)
	}
	for i := 0; i < 4; i++ {
		if !e.has(int64(10 + i)) {
			t.Fatal("aborted run still deleted")
		}
	}
	c := e.cfg(true)
	c.MaxPercent = 50 // the founder raises the limit on purpose
	if rep, _ = Run(context.Background(), e.v, c); rep.Deleted != 4 {
		t.Fatalf("with 50%% allowed: %+v", rep)
	}
}

func TestOpenDotaFailureDeletesNothing(t *testing.T) {
	e := setup(t)
	e.recent(t, 10)
	e.put(t, 1, vault.KindCoach, "58")
	e.od.Close()
	rep, err := Run(context.Background(), e.v, e.cfg(true))
	if err != nil || rep.Error == "" || rep.Deleted != 0 || !e.has(1) {
		t.Fatalf("report = %+v err=%v", rep, err)
	}
}

func TestUnknownPatchIsKeptUnlessItsMarkerSaysOtherwise(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.recent(t, 40)
	e.put(t, 1, vault.KindCoach, "") // no marker: kept
	e.put(t, 2, vault.KindCoach, "") // marker says May 2025: patch 58 -> goes
	e.put(t, 3, vault.KindCoach, "") // marker says April 2026: patch 60 -> stays
	e.v.WriteDone(ctx, "4.15.0", 2, vault.Done{OK: true, StartTime: float64(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC).Unix())})
	e.v.WriteDone(ctx, "4.15.1", 2, vault.Done{OK: true, StartTime: float64(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC).Unix())})
	e.v.WriteDone(ctx, "4.15.1", 3, vault.Done{OK: true, StartTime: float64(time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC).Unix())})
	rep, err := Run(ctx, e.v, e.cfg(true))
	if err != nil {
		t.Fatal(err)
	}
	if rep.UnknownPatch != 1 || rep.ResolvedPatch != 2 || rep.Deleted != 1 || !e.has(1) || e.has(2) || !e.has(3) {
		t.Fatalf("report = %+v 1=%v 2=%v 3=%v", rep, e.has(1), e.has(2), e.has(3))
	}
	o, _, _ := e.v.Find(ctx, 3)
	if o.Meta.Patch != strconv.Itoa(60) {
		t.Fatalf("resolved patch not written back: %q", o.Meta.Patch)
	}
}

func TestSemverLess(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{{"4.15.0", "4.15.1", true}, {"4.9.2", "4.15.0", true}, {"4.15.1", "4.15.1", false}, {"5.0.0", "4.99.9", false}} {
		if got := semverLess(tc.a, tc.b); got != tc.want {
			t.Errorf("%s < %s = %v", tc.a, tc.b, got)
		}
	}
	_ = fmt.Sprint
}

// raceStore lets a test change an object between retention's first look at it
// (the listing HEAD) and the second look right before the delete.
type raceStore struct {
	vault.Store
	mu        sync.Mutex
	heads     map[string]int
	onRecheck func(key string)
}

func (r *raceStore) Head(ctx context.Context, key string) (vault.ObjectInfo, error) {
	r.mu.Lock()
	r.heads[key]++
	second := r.heads[key] == 2
	r.mu.Unlock()
	if second && r.onRecheck != nil {
		r.onRecheck(key)
	}
	return r.Store.Head(ctx, key)
}

func TestDeleteRechecksTheObjectAfterTheListing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	rs := &raceStore{Store: e.mem, heads: map[string]int{}}
	e.v = vault.New(rs)
	e.recent(t, 40)
	e.put(t, 1, vault.KindCoach, "58") // upgraded to a student game after the listing
	e.put(t, 2, vault.KindCoach, "58") // untouched: goes
	e.put(t, 3, vault.KindCoach, "58") // re-dated to the current patch after the listing
	rs.onRecheck = func(key string) {
		switch key {
		case vault.ReplayKey(1):
			e.mem.CopyReplace(ctx, key, vault.Meta{Kind: vault.KindStudent, Patch: "58", Accounts: []string{"7"}}.ToS3())
		case vault.ReplayKey(3):
			e.mem.CopyReplace(ctx, key, vault.Meta{Kind: vault.KindCoach, Patch: "60"}.ToS3())
		}
	}
	rep, err := Run(ctx, e.v, e.cfg(true))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 1 || rep.ChangedSkip != 2 || !e.has(1) || e.has(2) || !e.has(3) {
		t.Fatalf("report = %+v 1=%v 2=%v 3=%v", rep, e.has(1), e.has(2), e.has(3))
	}
}

func TestAnUnreadableKindIsKept(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.recent(t, 40)
	e.put(t, 1, vault.KindCoach, "58")
	// an object without a kind (put by hand): it reads as unknown, not as a deletable coach game
	e.mem.CopyReplace(ctx, vault.ReplayKey(1), map[string]string{"patch": "58"})
	rep, _ := Run(ctx, e.v, e.cfg(true))
	if rep.Deleted != 0 || !e.has(1) {
		t.Fatalf("report = %+v", rep)
	}
}

func TestACoachCopyOfAProGameCannotShedItsProtection(t *testing.T) {
	e := setup(t)
	e.recent(t, 40)
	e.pins = []int64{1}
	e.put(t, 1, vault.KindCoach, "58") // stored first by a hint-less /parse-valve call
	if _, err := e.v.UpdateMeta(context.Background(), 1, vault.Meta{Kind: vault.KindPro}); err != nil {
		t.Fatal(err)
	}
	rep, _ := Run(context.Background(), e.v, e.cfg(true))
	if rep.Deleted != 0 || rep.PinnedPro != 1 || !e.has(1) {
		t.Fatalf("a pinned pro game was deleted: %+v", rep)
	}
}

// web specs/2026-10-30-prefetch-active-replays.md: the free players' games parsed ahead and unopened for 7 days go
// by the web's list — only those it names, only a student's game, dry-run unless PrefetchApply.
func TestPrefetchPurgeDeletesOnlyNamedStudentGames(t *testing.T) {
	e := setup(t)
	e.recent(t, 20)
	e.put(t, 7001, vault.KindStudent, "60") // named: goes
	e.put(t, 7002, vault.KindStudent, "60") // not named: stays
	e.put(t, 7003, vault.KindPro, "60")     // named but a pro game: stays
	e.purge, e.purgeOK = []int64{7001, 7003, 9999}, true

	dry, err := Run(context.Background(), e.v, e.cfg(false))
	if err != nil {
		t.Fatal(err)
	}
	if dry.Prefetch.Listed != 3 || dry.Prefetch.Candidates != 1 || dry.Prefetch.Deleted != 0 || !e.has(7001) {
		t.Fatalf("dry run: %+v", dry.Prefetch)
	}
	cfg := e.cfg(false)
	cfg.PrefetchApply = true
	rep, err := Run(context.Background(), e.v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Prefetch.Deleted != 1 || e.has(7001) || !e.has(7002) || !e.has(7003) {
		t.Fatalf("apply: %+v", rep.Prefetch)
	}
}

func TestPrefetchPurgeWithoutTheListDeletesNothing(t *testing.T) {
	e := setup(t)
	e.put(t, 7001, vault.KindStudent, "60")
	cfg := e.cfg(false)
	cfg.PrefetchApply = true
	rep, err := Run(context.Background(), e.v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Prefetch.Error == "" || rep.Prefetch.Deleted != 0 || !e.has(7001) {
		t.Fatalf("no list: %+v", rep.Prefetch)
	}
}
