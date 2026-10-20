package reparse

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"dota-replay-parser/internal/vault"
)

// ErrParseFailed: the parser rejected a replay that downloaded cleanly.
var ErrParseFailed = errors.New("parse failed")

// ParseFunc turns a stored replay into the parser's JSON. main supplies it
// (decompress, run the parser binary); the worker holds no parser or Valve code.
type ParseFunc func(ctx context.Context, replay io.Reader) ([]byte, error)

// Worker is one temporary machine's share of a run.
type Worker struct {
	V         *vault.Vault
	GH        GitHub
	Parse     ParseFunc
	HTTP      *http.Client
	Version   string
	PlanKey   string
	Shard     int
	Shards    int
	Parallel  int
	WebURL    string
	Token     string // VAULT_SERVICE_TOKEN, never logged
	Sleep     func(time.Duration)
	Backoff   time.Duration // base of the retry backoff (1s default)
	RoleLabel string
}

// Stats is what a worker did.
type Stats struct {
	Mine, Skipped, Delivered, Bad, Failed, Gone int64
	Dispatch                                    string
}

type fatalError struct{ error }

func (w *Worker) sleep(d time.Duration) {
	if w.Sleep != nil {
		w.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Run works through this shard's replays, then (if every replay of the plan
// has a marker) triggers the single page refresh. A non-nil error means some
// replay did not get a marker; the daily maintain resumes those.
func (w *Worker) Run(ctx context.Context) (Stats, error) {
	var st Stats
	if w.WebURL == "" || w.Token == "" {
		return st, errors.New("worker: WEB_URL and VAULT_SERVICE_TOKEN must be set")
	}
	var plan Plan
	ok, err := w.V.GetJSON(ctx, w.PlanKey, &plan)
	if err != nil {
		return st, fmt.Errorf("read plan: %w", err)
	}
	if !ok {
		return st, fmt.Errorf("plan %s not found", w.PlanKey)
	}
	done, err := w.V.DoneSet(ctx, w.Version)
	if err != nil {
		return st, err
	}
	var mine []PlanItem
	for _, it := range plan.Items {
		if Shard(it.ID, w.Shards) != w.Shard {
			continue
		}
		if done[it.ID] {
			st.Skipped++
			continue
		}
		mine = append(mine, it)
	}
	st.Mine = int64(len(mine))
	log.Printf("reparse: role=worker version=%s shard=%d/%d items=%d valve_requests=0", w.Version, w.Shard, w.Shards, len(mine))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	par := w.Parallel
	if par < 1 {
		par = 1
	}
	work := make(chan PlanItem)
	var wg sync.WaitGroup
	var fatal atomic.Value
	for i := 0; i < par; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range work {
				if ctx.Err() != nil {
					continue
				}
				switch err := w.one(ctx, it, &st); {
				case err == nil:
				case errors.As(err, new(fatalError)):
					fatal.Store(err)
					cancel()
				default:
					atomic.AddInt64(&st.Failed, 1)
					log.Printf("reparse: %d: %v", it.ID, err)
				}
			}
		}()
	}
	for _, it := range mine {
		work <- it
	}
	close(work)
	wg.Wait()

	if f, _ := fatal.Load().(error); f != nil {
		return st, f
	}
	if err := ctx.Err(); err != nil {
		return st, err
	}
	// Last one out dispatches: every replay of the plan has a marker.
	done, err = w.V.DoneSet(context.Background(), w.Version)
	if err != nil {
		return st, err
	}
	for _, it := range plan.Items {
		if !done[it.ID] {
			if st.Failed > 0 {
				return st, fmt.Errorf("%d replays left without a marker", st.Failed)
			}
			return st, nil // other machines are still working
		}
	}
	d, err := Dispatch(context.Background(), w.V, w.GH, w.Version)
	st.Dispatch = d
	if err != nil {
		return st, fmt.Errorf("dispatch: %w", err)
	}
	log.Printf("reparse: dispatch=%s", d)
	return st, nil
}

func (w *Worker) one(ctx context.Context, it PlanItem, st *Stats) error {
	start := time.Now()
	rc, meta, err := w.V.Open(ctx, it.ID)
	if errors.Is(err, vault.ErrNotFound) {
		atomic.AddInt64(&st.Gone, 1) // retention removed it after the plan was made
		return nil
	}
	if err != nil {
		return err
	}
	out, perr := w.Parse(ctx, rc)
	rc.Close()
	if perr != nil {
		if errors.Is(perr, ErrParseFailed) {
			if _, err := w.V.UpdateMeta(ctx, it.ID, vault.Meta{Kind: meta.Kind, Bad: true}); err != nil {
				log.Printf("reparse: %d: set bad: %v", it.ID, err)
			}
			atomic.AddInt64(&st.Bad, 1)
			return w.V.WriteDone(ctx, w.Version, it.ID, vault.Done{Kind: meta.Kind, Final: true, Error: "parse_failed", Role: "worker"})
		}
		return perr
	}
	var head struct {
		StartDateTime int64 `json:"startDateTime"`
	}
	_ = json.Unmarshal(out, &head)

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(out)
	zw.Close()

	kind := meta.Kind
	mode := kind.DeliveryMode()
	resp, derr := w.deliver(ctx, it.ID, kind, mode, gz.Bytes())
	if derr != nil {
		var pe permanentError
		if errors.As(derr, &pe) {
			atomic.AddInt64(&st.Failed, 1)
			return w.V.WriteDone(ctx, w.Version, it.ID, vault.Done{Kind: kind, Final: true, Mode: mode,
				Error: pe.Error(), Role: "worker"})
		}
		return derr
	}
	atomic.AddInt64(&st.Delivered, 1)
	return w.V.WriteDone(ctx, w.Version, it.ID, vault.Done{
		OK: true, Kind: kind, Mode: mode, Ms: time.Since(start).Milliseconds(), Bytes: int64(len(out)),
		StartTime: float64(head.StartDateTime), Skipped: resp.Skipped, Role: "worker",
	})
}

type permanentError struct{ msg string }

func (e permanentError) Error() string { return e.msg }

type deliverResp struct {
	OK      bool            `json:"ok"`
	Written json.RawMessage `json:"written"`
	Skipped string          `json:"skipped"`
}

// deliver posts one gzip parse to the web app. 5xx, 429 and network errors
// retry 3 times with backoff; 401/403/404/405 stop the whole worker; other 4xx
// are final for that replay.
func (w *Worker) deliver(ctx context.Context, id int64, kind vault.Kind, mode string, gz []byte) (deliverResp, error) {
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	target := w.WebURL + "/api/internal/parse-result?mode=" + url.QueryEscape(mode)
	base := w.Backoff
	if base <= 0 {
		base = time.Second
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			w.sleep(base * time.Duration(1<<(2*(attempt-1)))) // 1x, 4x
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(gz))
		if err != nil {
			return deliverResp{}, err
		}
		req.Header.Set("Authorization", "Bearer "+w.Token)
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set("X-Match-Id", fmt.Sprint(id))
		req.Header.Set("X-Vault-Kind", string(kind))
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return deliverResp{}, ctx.Err()
			}
			last = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			var dr deliverResp
			if err := json.Unmarshal(body, &dr); err != nil || !dr.OK {
				last = fmt.Errorf("web answered %d without ok", resp.StatusCode)
				continue
			}
			return dr, nil
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return deliverResp{}, fatalError{fmt.Errorf("web refused the service token (%d)", resp.StatusCode)}
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
			// the route is not deployed: marking every replay failed would hide that
			return deliverResp{}, fatalError{fmt.Errorf("web has no parse-result route (%d)", resp.StatusCode)}
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout ||
			resp.StatusCode >= 500:
			last = fmt.Errorf("web answered %d", resp.StatusCode)
		default:
			return deliverResp{}, permanentError{fmt.Sprintf("web_%d", resp.StatusCode)}
		}
	}
	return deliverResp{}, last
}
