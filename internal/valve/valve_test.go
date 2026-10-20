package valve

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dota-replay-parser/internal/vault"
)

const zstHex = "28b52ffd2417b90000504244454d53320068656c6c6f2d64656d6f2d626f64794c58d1ad"

func newFetcher(t *testing.T, h http.HandlerFunc) (*Fetcher, *vault.Vault, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	v := vault.New(vault.NewMemStore())
	return &Fetcher{Client: srv.Client(), Vault: v, Role: "server",
		URLFor: func(c int, m, s int64) string { return fmt.Sprintf("%s/%d_%d_%d", srv.URL, c, m, s) }}, v, &hits
}

func TestURLFormat(t *testing.T) {
	if got := URL(171, 8918319751, 456); got != "http://replay171.valve.net/570/8918319751_456.dem.bz2" {
		t.Fatalf("URL = %s", got)
	}
}

func TestStoreMissThenPresent(t *testing.T) {
	zst, _ := hex.DecodeString(zstHex)
	f, v, hits := newFetcher(t, func(w http.ResponseWriter, r *http.Request) { w.Write(zst) })
	ctx := context.Background()
	it := Item{MatchID: 11, Cluster: 1, Salt: 2, Meta: vault.Meta{Kind: vault.KindLadder, Patch: "60"}}
	if r := f.Store(ctx, v, "backfill", it); r.Status != StatusStored {
		t.Fatalf("first = %+v", r)
	}
	o, ok, _ := v.Find(ctx, 11)
	if !ok || o.Meta.Kind != vault.KindLadder || o.Meta.Container != vault.ContainerZst || o.Meta.Source != "valve-backfill" {
		t.Fatalf("stored meta = %+v ok=%v", o.Meta, ok)
	}
	// second time: no Valve request, accounts merged
	it.Meta.Accounts = []string{"9"}
	if r := f.Store(ctx, v, "backfill", it); r.Status != StatusPresent {
		t.Fatalf("second = %+v", r)
	}
	if hits.Load() != 1 {
		t.Fatalf("valve hits = %d, want 1", hits.Load())
	}
	o, _, _ = v.Find(ctx, 11)
	if strings.Join(o.Meta.Accounts, ",") != "9" {
		t.Fatalf("accounts not merged: %+v", o.Meta)
	}
	if c, _ := v.CountValve(ctx, time.Now().Add(-time.Hour)); c["server"] != 1 {
		t.Fatalf("valvelog = %v", c)
	}
}

func TestStoreStatuses(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{{404, StatusUnavailable}, {403, StatusUnavailable}, {410, StatusUnavailable}, {503, StatusFailed}, {429, StatusFailed}} {
		f, v, _ := newFetcher(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code) })
		r := f.Store(context.Background(), v, "backfill", Item{MatchID: 1, Cluster: 1, Salt: 1, Meta: vault.Meta{Kind: vault.KindLadder}})
		if r.Status != tc.want || r.ValveStatus != tc.code {
			t.Errorf("valve %d => %+v, want %s", tc.code, r, tc.want)
		}
		if _, ok, _ := v.Find(context.Background(), 1); ok {
			t.Errorf("valve %d stored something", tc.code)
		}
	}
}

func TestTruncatedAndGarbageAreNotStored(t *testing.T) {
	zst, _ := hex.DecodeString(zstHex)
	// Content-Length says more than is sent: the connection drops.
	f, v, _ := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(zst)+100))
		w.Write(zst)
	})
	if r := f.Store(context.Background(), v, "backfill", Item{MatchID: 2, Cluster: 1, Salt: 1}); r.Status != StatusFailed {
		t.Fatalf("truncated = %+v", r)
	}
	f2, v2, _ := newFetcher(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>error page</html>")) })
	if r := f2.Store(context.Background(), v2, "backfill", Item{MatchID: 3, Cluster: 1, Salt: 1}); r.Status != StatusFailed {
		t.Fatalf("garbage = %+v", r)
	}
	for _, vv := range []*vault.Vault{v, v2} {
		ids, _ := vv.ListIDs(context.Background())
		if len(ids) != 0 {
			t.Fatalf("bad download was stored: %v", ids)
		}
	}
}

func TestDownloadCapIsAnErrorNotATruncation(t *testing.T) {
	f, _, _ := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Write(make([]byte, 4096))
		w.(http.Flusher).Flush()
		w.Write(make([]byte, 4096))
	})
	f.MaxBytes = 1000
	if _, err := f.Download(context.Background(), "t", 1, 1, 1); err == nil {
		t.Fatal("body over the cap must fail")
	}
	tmp, _ := os.ReadDir(os.TempDir())
	for _, e := range tmp {
		if strings.HasPrefix(e.Name(), "replay-raw-") {
			// leftover from a failed download would be a leak (other tests clean up theirs)
			info, _ := e.Info()
			if time.Since(info.ModTime()) < 5*time.Second && info.Size() == 1000 {
				t.Fatalf("partial file left behind: %s", e.Name())
			}
		}
	}
}

func TestStoreReplacesAnObjectMarkedCorrupt(t *testing.T) {
	zst, _ := hex.DecodeString(zstHex)
	f, v, hits := newFetcher(t, func(w http.ResponseWriter, r *http.Request) { w.Write(zst) })
	ctx := context.Background()
	v.PutNew(ctx, 21, strings.NewReader("truncated junk"), 14, vault.Meta{Kind: vault.KindLadder, Patch: "60", Container: vault.ContainerBz2})
	it := Item{MatchID: 21, Cluster: 1, Salt: 2, Meta: vault.Meta{Kind: vault.KindLadder}}
	// healthy as far as anyone knows: present, no fetch
	if r := f.Store(ctx, v, "backfill", it); r.Status != StatusPresent || hits.Load() != 0 {
		t.Fatalf("unflagged = %+v hits=%d", r, hits.Load())
	}
	if err := v.MarkCorrupt(ctx, 21); err != nil {
		t.Fatal(err)
	}
	if r := f.Store(ctx, v, "backfill", it); r.Status != StatusStored || hits.Load() != 1 {
		t.Fatalf("corrupt = %+v hits=%d", r, hits.Load())
	}
	o, _, _ := v.Find(ctx, 21)
	if o.Meta.Corrupt || o.Meta.Container != vault.ContainerZst || o.Meta.Patch != "60" || o.Info.Size != int64(len(zst)) {
		t.Fatalf("after replace = %+v size %d", o.Meta, o.Info.Size)
	}
}

type cancelOnEOF struct {
	r      *strings.Reader
	cancel context.CancelFunc
}

func (c cancelOnEOF) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil {
		c.cancel() // the caller hangs up just as the download ends
	}
	return n, err
}
func (c cancelOnEOF) Close() error { return nil }

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStoreKeepsAFinishedDownloadWhenTheCallerHangsUp(t *testing.T) {
	zst, _ := hex.DecodeString(zstHex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := vault.New(vault.NewMemStore())
	f := &Fetcher{Vault: v, Role: "server", Client: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: int64(len(zst)), Header: http.Header{}, Request: r,
			Body: cancelOnEOF{r: strings.NewReader(string(zst)), cancel: cancel}}, nil
	})}}
	r := f.Store(ctx, v, "backfill", Item{MatchID: 22, Cluster: 1, Salt: 2, Meta: vault.Meta{Kind: vault.KindLadder}})
	if r.Status != StatusStored {
		t.Fatalf("a cancelled request dropped a finished download: %+v", r)
	}
	if _, ok, _ := v.Find(context.Background(), 22); !ok {
		t.Fatal("not stored")
	}
}
