package vault

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotFound: the key does not exist.
	ErrNotFound = errors.New("vault: not found")
	// ErrExists: a conditional create found the key already there.
	ErrExists = errors.New("vault: already exists")
)

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	Meta         map[string]string // lowercase user metadata; empty from List
	LastModified time.Time
}

// PutOpts are the options of a Put.
type PutOpts struct {
	Meta        map[string]string
	IfNoneMatch bool // conditional create: ErrExists when the key is there
	ContentType string
}

// Store is the object-store seam: S3Store in production, MemStore in tests.
type Store interface {
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64, opts PutOpts) error
	// CopyReplace rewrites the metadata of an existing key in place.
	CopyReplace(ctx context.Context, key string, meta map[string]string) error
	Delete(ctx context.Context, key string) error
	// List calls fn for every key under prefix (no metadata).
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
}
