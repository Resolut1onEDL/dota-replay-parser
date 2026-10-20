package vault

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

type memObject struct {
	data []byte
	meta map[string]string
	at   time.Time
	etag string
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu      sync.Mutex
	seq     int
	objects map[string]*memObject
	// Gets counts Get calls per key (tests assert "no fetch").
	Gets map[string]int
	// FailPut, when set, makes Put fail for keys it returns true for.
	FailPut func(key string) error
	// BeforeCopy, when set, runs at the start of every CopyReplaceIfMatch.
	BeforeCopy func(key string)
}

func NewMemStore() *MemStore {
	return &MemStore{objects: map[string]*memObject{}, Gets: map[string]int{}}
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

// nextETag must be called with s.mu held.
func (s *MemStore) nextETag() string {
	s.seq++
	return fmt.Sprintf(`"mem-%d"`, s.seq)
}

func (s *MemStore) Head(_ context.Context, key string) (ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: int64(len(o.data)), Meta: copyMap(o.meta), LastModified: o.at, ETag: o.etag}, nil
}

func (s *MemStore) Get(_ context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	s.Gets[key]++
	return io.NopCloser(bytes.NewReader(append([]byte(nil), o.data...))),
		ObjectInfo{Key: key, Size: int64(len(o.data)), Meta: copyMap(o.meta), LastModified: o.at, ETag: o.etag}, nil
}

func (s *MemStore) Put(ctx context.Context, key string, body io.ReadSeeker, _ int64, opts PutOpts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailPut != nil {
		if err := s.FailPut(key); err != nil {
			return err
		}
	}
	if _, ok := s.objects[key]; ok && opts.IfNoneMatch {
		return ErrExists
	}
	s.objects[key] = &memObject{data: data, meta: copyMap(opts.Meta), at: time.Now(), etag: s.nextETag()}
	return nil
}

func (s *MemStore) CopyReplace(_ context.Context, key string, meta map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return ErrNotFound
	}
	o.meta = copyMap(meta)
	o.etag = s.nextETag()
	return nil
}

// CopyReplaceIfMatch is the conditional rewrite; BeforeCopy (when set) runs
// first, outside the lock, so a test can change the object "between" the read
// and the write.
func (s *MemStore) CopyReplaceIfMatch(_ context.Context, key, etag string, meta map[string]string) error {
	if s.BeforeCopy != nil {
		s.BeforeCopy(key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return ErrNotFound
	}
	if etag != "" && o.etag != etag {
		return ErrChanged
	}
	o.meta = copyMap(meta)
	o.etag = s.nextETag()
	return nil
}

func (s *MemStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *MemStore) List(_ context.Context, prefix string, fn func(ObjectInfo) error) error {
	s.mu.Lock()
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	infos := make([]ObjectInfo, 0, len(keys))
	for _, k := range keys {
		o := s.objects[k]
		infos = append(infos, ObjectInfo{Key: k, Size: int64(len(o.data)), LastModified: o.at})
	}
	s.mu.Unlock()
	for _, in := range infos {
		if err := fn(in); err != nil {
			return err
		}
	}
	return nil
}

// Keys returns all keys under prefix (test helper).
func (s *MemStore) Keys(prefix string) []string {
	var out []string
	_ = s.List(context.Background(), prefix, func(o ObjectInfo) error { out = append(out, o.Key); return nil })
	return out
}
