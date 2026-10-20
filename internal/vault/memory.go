package vault

import (
	"bytes"
	"context"
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
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu      sync.Mutex
	objects map[string]*memObject
	// Gets counts Get calls per key (tests assert "no fetch").
	Gets map[string]int
	// FailPut, when set, makes Put fail for keys it returns true for.
	FailPut func(key string) error
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

func (s *MemStore) Head(_ context.Context, key string) (ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: int64(len(o.data)), Meta: copyMap(o.meta), LastModified: o.at}, nil
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
		ObjectInfo{Key: key, Size: int64(len(o.data)), Meta: copyMap(o.meta), LastModified: o.at}, nil
}

func (s *MemStore) Put(_ context.Context, key string, body io.ReadSeeker, _ int64, opts PutOpts) error {
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
	s.objects[key] = &memObject{data: data, meta: copyMap(opts.Meta), at: time.Now()}
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
