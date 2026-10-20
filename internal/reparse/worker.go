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

// ErrParseFailed: the parser rejected a replay that downloaded cleanly, or the
// stored bytes do not decode. Nothing else may be mapped to it: it flags the
// replay bad.
var ErrParseFailed = errors.New("parse failed")

const (
	// defaultMaxFailures: a replay that fails this many separate attempts for
	// reasons of its own gets a final marker, so it cannot hold up the run.
	defaultMaxFailures = 3
	// defaultMaxBody is the largest gzip body sent to the web app: the Vercel
	// function limit is 4.5 MB and a full parse gzips to about 1 MB.
	defaultMaxBody = 4_000_000
)

// deliveryError marks a failure of the web side (5xx, 429, network). It says
// nothing about the replay, so it never counts toward giving up on it.
type deliveryError struct{ error }

func (e deliveryError) Unwrap() error { return e.error }

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
	// MaxFailures and MaxBody default to defaultMaxFailures and defaultMaxBody.
	MaxFailures int
	MaxBody     int
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
					w.recordFailure(ctx, it, err)
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
	// Last one out dispatches: every replay of the plan has a marker, or is
	// gone (retention removed it after the plan was made, so it never will).
	done, err = w.V.DoneSet(context.Background(), w.Version)
	if err != nil {
		return st, err
	}
	left := 0
	for _, it := range plan.Items {
		if done[it.ID] {
			continue
		}
		if present, err := w.V.Exists(context.Background(), vault.ReplayKey(it.ID)); err == nil && !present {
			continue
		}
		left++
	}
	if left > 0 {
		if st.Failed > 0 {
			return st, fmt.Errorf("%d replays left without a marker", left)
		}
		return st, nil // other machines are still working
	}
	d, err := Dispatch(context.Background(), w.V, w.GH, w.Version)
	st.Dispatch = d
	if err != nil {
		return st, fmt.Errorf("dispatch: %w", err)
	}
	log.Printf("reparse: dispatch=%s", d)
	return st, nil
}

// recordFailure counts a failed attempt at a replay and, once the limit is
// reached, closes it with a final marker so the run can finish. Failures of
// the web side do not count: when it is down every replay fails, and none of
// them should be given up on.
func (w *Worker) recordFailure(ctx context.Context, it PlanItem, cause error) {
	var de deliveryError
	if errors.As(cause, &de) || ctx.Err() != nil {
		return
	}
	n, err := w.V.BumpFailure(ctx, w.Version, it.ID, cause.Error())
	if err != nil {
		log.Printf("reparse: %d: count failure: %v", it.ID, err)
		return
	}
	limit := w.MaxFailures
	if limit < 1 {
		limit = defaultMaxFailures
	}
	if n < limit {
		return
	}
	log.Printf("reparse: %d: giving up after %d failed attempts: %v", it.ID, n, cause)
	if err := w.V.WriteDone(ctx, w.Version, it.ID, vault.Done{Kind: it.Kind, Final: true, Error: "gave_up", Role: "worker"}); err != nil {
		log.Printf("reparse: %d: write final marker: %v", it.ID, err)
	}
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
			if err := w.V.MarkBad(ctx, it.ID, w.Version); err != nil {
				log.Printf("reparse: %d: set bad: %v", it.ID, err)
			}
			atomic.AddInt64(&st.Bad, 1)
			return w.V.WriteDone(ctx, w.Version, it.ID, vault.Done{Kind: meta.Kind, Final: true, Error: "parse_failed", Role: "worker"})
		}
		return perr
	}
	if meta.Bad {
		// the parser took it this time: an earlier verdict (an older release) is void
		if err := w.V.ClearBad(ctx, it.ID); err != nil {
			log.Printf("reparse: %d: clear bad: %v", it.ID, err)
		}
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
	limit := w.MaxBody
	if limit <= 0 {
		limit = defaultMaxBody
	}
	if gz.Len() > limit {
		// the web would answer 413: do not send it, and say so in the marker
		atomic.AddInt64(&st.Failed, 1)
		log.Printf("reparse: %d: parse is %d bytes gzipped, over the %d limit", it.ID, gz.Len(), limit)
		return w.V.WriteDone(ctx, w.Version, it.ID, vault.Done{Kind: kind, Final: true, Mode: mode,
			Bytes: int64(len(out)), Error: "too_large", Role: "worker"})
	}
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
	return deliverResp{}, deliveryError{last}
}
