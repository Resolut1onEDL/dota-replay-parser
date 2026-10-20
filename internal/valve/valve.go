// Package valve is the only code that fetches replays from Valve's CDN into
// the vault. internal/reparse (the worker) must never import it: a re-parse
// reads the vault only, and a test proves that with go list -deps.
package valve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"dota-replay-parser/internal/vault"
)

// DefaultMaxBytes caps one download (the longest games are ~300 MB compressed).
const DefaultMaxBytes = 700 << 20

// URL is where Valve serves a replay (the .bz2 name is historic: the bytes
// may be bzip2, zstd or raw).
func URL(cluster int, matchID, salt int64) string {
	return fmt.Sprintf("http://replay%d.valve.net/570/%d_%d.dem.bz2", cluster, matchID, salt)
}

// StatusError: Valve answered, but not 200.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("valve returned %d", e.Status) }

// Unavailable says the replay is gone for good (403, 404, 410) rather than a
// transient Valve failure.
func (e *StatusError) Unavailable() bool {
	return e.Status == http.StatusForbidden || e.Status == http.StatusNotFound || e.Status == http.StatusGone
}

// ErrTransport: the request never got an answer.
var ErrTransport = errors.New("valve unreachable")

// ErrIncomplete: the body ended early, was too long or failed to read; nothing
// of it is kept.
var ErrIncomplete = errors.New("valve download incomplete")

// Fetcher downloads replays and keeps the Valve-fetch log.
type Fetcher struct {
	Client   *http.Client
	Vault    *vault.Vault // valvelog; nil = no log
	Role     string       // server | worker-less tools
	MaxBytes int64
	// URLFor overrides URL (tests).
	URLFor func(cluster int, matchID, salt int64) string
}

// Result is a finished download: Path holds the bytes exactly as Valve served
// them; the caller removes it.
type Result struct {
	Path    string
	Size    int64
	Elapsed time.Duration
}

type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, ErrIncomplete
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

func (f *Fetcher) log(source string, id int64, status int) {
	if f.Vault == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = f.Vault.LogValve(ctx, f.Role, source, id, status)
}

// Download does one GET into a temp file. The file is complete or absent: a
// length mismatch, a read error or a body over the cap is an error and the
// partial file is removed.
func (f *Fetcher) Download(ctx context.Context, source string, matchID int64, cluster int, salt int64) (*Result, error) {
	urlFor := f.URLFor
	if urlFor == nil {
		urlFor = URL
	}
	max := f.MaxBytes
	if max <= 0 {
		max = DefaultMaxBytes
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlFor(cluster, matchID, salt), nil)
	var resp *http.Response
	if err == nil {
		resp, err = f.Client.Do(req)
	}
	if err != nil {
		f.log(source, matchID, 0)
		return nil, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	defer resp.Body.Close()
	f.log(source, matchID, resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode}
	}
	tmp, err := os.CreateTemp("", "replay-raw-*")
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(tmp, &capReader{r: resp.Body, left: max})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		err = fmt.Errorf("%w: got %d of %d bytes", ErrIncomplete, n, resp.ContentLength)
	}
	if err != nil {
		os.Remove(tmp.Name())
		if errors.Is(err, ErrIncomplete) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrIncomplete, err)
	}
	return &Result{Path: tmp.Name(), Size: n, Elapsed: time.Since(start)}, nil
}

// VerifyFile checks the downloaded bytes decompress into a demo.
func VerifyFile(path string) (container string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	container, _, err = vault.Verify(f)
	return container, err
}

// Item is one replay to store without parsing.
type Item struct {
	MatchID int64
	Cluster int
	Salt    int64
	Meta    vault.Meta
}

// Statuses of a backfill item.
const (
	StatusStored      = "stored"
	StatusPresent     = "present"
	StatusUnavailable = "unavailable"
	StatusFailed      = "failed"
)

// ItemResult is the outcome of one backfill item.
type ItemResult struct {
	MatchID     int64  `json:"match_id"`
	Status      string `json:"status"`
	ValveStatus int    `json:"valve_status,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Store puts one replay into the vault without parsing it: skip if present
// (merging the metadata), else download, verify and store.
func (f *Fetcher) Store(ctx context.Context, v *vault.Vault, source string, it Item) ItemResult {
	res := ItemResult{MatchID: it.MatchID}
	if it.Meta.Source == "" {
		it.Meta.Source = "valve-backfill"
	}
	if _, ok, err := v.Find(ctx, it.MatchID); err != nil {
		res.Status, res.Error = StatusFailed, "vault: "+err.Error()
		return res
	} else if ok {
		if _, err := v.UpdateMeta(ctx, it.MatchID, it.Meta); err != nil {
			res.Status, res.Error = StatusFailed, "vault: "+err.Error()
			return res
		}
		res.Status = StatusPresent
		return res
	}
	d, err := f.Download(ctx, source, it.MatchID, it.Cluster, it.Salt)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			res.ValveStatus = se.Status
			if se.Unavailable() {
				res.Status = StatusUnavailable
				return res
			}
		}
		res.Status, res.Error = StatusFailed, err.Error()
		return res
	}
	defer os.Remove(d.Path)
	container, err := VerifyFile(d.Path)
	if err != nil {
		res.Status, res.Error = StatusFailed, "bad replay: "+err.Error()
		return res
	}
	it.Meta.Container = container
	file, err := os.Open(d.Path)
	if err != nil {
		res.Status, res.Error = StatusFailed, err.Error()
		return res
	}
	defer file.Close()
	if _, err := v.PutNew(ctx, it.MatchID, file, d.Size, it.Meta); err != nil {
		res.Status, res.Error = StatusFailed, "vault: "+err.Error()
		return res
	}
	res.Status = StatusStored
	return res
}
