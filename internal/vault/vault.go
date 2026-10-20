package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	replayPrefix  = "replays/"
	donePrefix    = "_vault/done/"
	runsPrefix    = "_vault/runs/"
	valvelogPref  = "_vault/valvelog/"
	retentionPref = "_vault/retention/"
)

// ReplayKey is the object key of a match.
func ReplayKey(id int64) string { return replayPrefix + strconv.FormatInt(id, 10) + ".dem" }

// ParseReplayKey is the inverse of ReplayKey.
func ParseReplayKey(key string) (int64, bool) {
	if !strings.HasPrefix(key, replayPrefix) || !strings.HasSuffix(key, ".dem") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(key, replayPrefix), ".dem"), 10, 64)
	return n, err == nil && n > 0
}

func DonePrefix(ver string) string        { return donePrefix + ver + "/" }
func DoneKey(ver string, id int64) string { return DonePrefix(ver) + strconv.FormatInt(id, 10) }
func PlanKey(ver string) string           { return runsPrefix + ver + "/plan.json" }
func AttemptKey(ver, stamp string) string { return runsPrefix + ver + "/attempt-" + stamp + ".json" }
func DispatchedKey(ver string) string     { return runsPrefix + ver + "/dispatched" }
func RetentionKey(date string) string     { return retentionPref + date + ".json" }
func ValvelogPrefix(day string) string    { return valvelogPref + day + "/" }
func RunsPrefix(ver string) string        { return runsPrefix + ver + "/" }
func DoneRoot() string                    { return donePrefix }

// Object is a stored replay with its parsed metadata.
type Object struct {
	ID   int64
	Info ObjectInfo
	Meta Meta
}

// Vault is the replay store plus its control objects.
type Vault struct {
	S Store
}

func New(s Store) *Vault { return &Vault{S: s} }

// Find does a HEAD; ok=false when the replay is not stored.
func (v *Vault) Find(ctx context.Context, id int64) (Object, bool, error) {
	info, err := v.S.Head(ctx, ReplayKey(id))
	if errors.Is(err, ErrNotFound) {
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, err
	}
	return Object{ID: id, Info: info, Meta: MetaFromS3(info.Meta)}, true, nil
}

// Open streams the stored bytes.
func (v *Vault) Open(ctx context.Context, id int64) (io.ReadCloser, Meta, error) {
	rc, info, err := v.S.Get(ctx, ReplayKey(id))
	if err != nil {
		return nil, Meta{}, err
	}
	return rc, MetaFromS3(info.Meta), nil
}

// UpdateMeta merges want into the stored metadata and rewrites it only when
// something changed (read-modify-write; the merge is monotone, so a lost race
// only delays a union).
func (v *Vault) UpdateMeta(ctx context.Context, id int64, want Meta) (Meta, error) {
	o, ok, err := v.Find(ctx, id)
	if err != nil {
		return Meta{}, err
	}
	if !ok {
		return Meta{}, ErrNotFound
	}
	merged, changed := Merge(o.Meta, want)
	if !changed {
		return merged, nil
	}
	return merged, v.S.CopyReplace(ctx, ReplayKey(id), merged.ToS3())
}

// PutNew stores the bytes under the match key if it is free. When the key is
// taken the metadata is merged instead and stored=false.
func (v *Vault) PutNew(ctx context.Context, id int64, body io.ReadSeeker, size int64, m Meta) (stored bool, err error) {
	if m.StoredAt == "" {
		m.StoredAt = Now().Format(time.RFC3339)
	}
	if m.Kind == "" {
		m.Kind = KindCoach
	}
	err = v.S.Put(ctx, ReplayKey(id), body, size, PutOpts{Meta: m.ToS3(), IfNoneMatch: true, ContentType: "application/octet-stream"})
	if errors.Is(err, ErrExists) {
		_, merr := v.UpdateMeta(ctx, id, m)
		return false, merr
	}
	return err == nil, err
}

// ListIDs returns the stored match ids, ascending.
func (v *Vault) ListIDs(ctx context.Context) ([]int64, error) {
	var ids []int64
	err := v.S.List(ctx, replayPrefix, func(o ObjectInfo) error {
		if id, ok := ParseReplayKey(o.Key); ok {
			ids = append(ids, id)
		}
		return nil
	})
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, err
}

// Objects HEADs the given ids (16 in parallel) for their metadata. Ids that
// disappeared meanwhile are left out.
func (v *Vault) Objects(ctx context.Context, ids []int64) ([]Object, error) {
	out := make([]Object, len(ids))
	have := make([]bool, len(ids))
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id int64) {
			defer wg.Done()
			defer func() { <-sem }()
			o, ok, err := v.Find(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			out[i], have[i] = o, ok
		}(i, id)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	res := make([]Object, 0, len(ids))
	for i := range out {
		if have[i] {
			res = append(res, out[i])
		}
	}
	return res, nil
}

// PutJSON writes a small control object.
func (v *Vault) PutJSON(ctx context.Context, key string, val any) error {
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return v.S.Put(ctx, key, bytes.NewReader(b), int64(len(b)), PutOpts{ContentType: "application/json"})
}

// PutJSONIfAbsent is a conditional create: created=false when the key exists.
func (v *Vault) PutJSONIfAbsent(ctx context.Context, key string, val any) (created bool, err error) {
	b, err := json.Marshal(val)
	if err != nil {
		return false, err
	}
	err = v.S.Put(ctx, key, bytes.NewReader(b), int64(len(b)), PutOpts{ContentType: "application/json", IfNoneMatch: true})
	if errors.Is(err, ErrExists) {
		return false, nil
	}
	return err == nil, err
}

// GetJSON reads a control object; ok=false when it does not exist.
func (v *Vault) GetJSON(ctx context.Context, key string, into any) (bool, error) {
	rc, _, err := v.S.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer rc.Close()
	return true, json.NewDecoder(rc).Decode(into)
}

// Exists is a HEAD on any key.
func (v *Vault) Exists(ctx context.Context, key string) (bool, error) {
	_, err := v.S.Head(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Done is the marker a worker leaves when one match is finished for a parser
// version. Presence alone means "do not do it again".
type Done struct {
	OK        bool    `json:"ok"`
	Final     bool    `json:"final,omitempty"`
	Kind      Kind    `json:"kind"`
	Mode      string  `json:"mode,omitempty"`
	Ms        int64   `json:"ms,omitempty"`
	Bytes     int64   `json:"bytes,omitempty"`
	StartTime float64 `json:"startTime,omitempty"` // unix seconds from the parse
	Skipped   string  `json:"skipped,omitempty"`
	Error     string  `json:"error,omitempty"`
	At        string  `json:"at"`
	Role      string  `json:"role,omitempty"`
}

func (v *Vault) WriteDone(ctx context.Context, ver string, id int64, d Done) error {
	if d.At == "" {
		d.At = Now().Format(time.RFC3339)
	}
	return v.PutJSON(ctx, DoneKey(ver, id), d)
}

func (v *Vault) ReadDone(ctx context.Context, ver string, id int64) (Done, bool, error) {
	var d Done
	ok, err := v.GetJSON(ctx, DoneKey(ver, id), &d)
	return d, ok, err
}

// DoneSet lists the ids that have a marker for ver.
func (v *Vault) DoneSet(ctx context.Context, ver string) (map[int64]bool, error) {
	set := map[int64]bool{}
	prefix := DonePrefix(ver)
	err := v.S.List(ctx, prefix, func(o ObjectInfo) error {
		if id, err := strconv.ParseInt(strings.TrimPrefix(o.Key, prefix), 10, 64); err == nil {
			set[id] = true
		}
		return nil
	})
	return set, err
}

// ValveLogEntry is one real GET to Valve, kept as an object so it survives
// restarts: the proof that a re-parse downloaded nothing.
type ValveLogEntry struct {
	Role   string `json:"role"`
	Source string `json:"source"`
	ID     int64  `json:"id"`
	Status int    `json:"status"`
	At     string `json:"at"`
}

// LogValve records a Valve GET; failures are the caller's to ignore.
func (v *Vault) LogValve(ctx context.Context, role, source string, id int64, status int) error {
	now := Now()
	e := ValveLogEntry{Role: role, Source: source, ID: id, Status: status, At: now.Format(time.RFC3339)}
	key := fmt.Sprintf("%s%d-%s-%d", ValvelogPrefix(now.Format("20060102")), now.UnixNano(), role, id)
	return v.PutJSON(ctx, key, e)
}

// CountValve counts Valve GETs since a moment, per role. The key is
// <unixnano>-<role>-<id> under a per-day prefix, so no object is read.
func (v *Vault) CountValve(ctx context.Context, since time.Time) (map[string]int, error) {
	counts := map[string]int{}
	now := Now()
	for d := since.UTC().Truncate(24 * time.Hour); !d.After(now); d = d.Add(24 * time.Hour) {
		prefix := ValvelogPrefix(d.Format("20060102"))
		if err := v.S.List(ctx, prefix, func(o ObjectInfo) error {
			parts := strings.SplitN(strings.TrimPrefix(o.Key, prefix), "-", 3)
			if len(parts) != 3 {
				return nil
			}
			if ns, err := strconv.ParseInt(parts[0], 10, 64); err == nil && ns >= since.UnixNano() {
				counts[parts[1]]++
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return counts, nil
}
