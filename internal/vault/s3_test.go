package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is just enough of the S3 wire protocol (path-style) to prove the
// store's requests and its error mapping: PUT with If-None-Match, same-key
// COPY with REPLACE, HEAD/GET/DELETE and ListObjectsV2.
type fakeS3 struct {
	mu          sync.Mutex
	objs        map[string]*fakeObj
	rejectCopy  bool // refuse a copy onto the same key (the fallback path)
	sawIfNone   bool
	sawChecksum bool
}

type fakeObj struct {
	data []byte
	meta map[string]string
}

func s3Err(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != "bkt" {
		s3Err(w, 404, "NoSuchBucket")
		return
	}
	meta := map[string]string{}
	for k, v := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			meta[strings.ToLower(strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-"))] = v[0]
		}
	}
	switch r.Method {
	case http.MethodGet:
		if key == "" { // list
			prefix := r.URL.Query().Get("prefix")
			var keys []string
			for k := range f.objs {
				if strings.HasPrefix(k, prefix) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			var sb strings.Builder
			sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>bkt</Name><IsTruncated>false</IsTruncated>`)
			for _, k := range keys {
				fmt.Fprintf(&sb, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-10-20T10:00:00.000Z</LastModified></Contents>`, k, len(f.objs[k].data))
			}
			sb.WriteString(`</ListBucketResult>`)
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, sb.String())
			return
		}
		o, ok := f.objs[key]
		if !ok {
			s3Err(w, 404, "NoSuchKey")
			return
		}
		for k, v := range o.meta {
			w.Header().Set("x-amz-meta-"+k, v)
		}
		w.Write(o.data)
	case http.MethodHead:
		o, ok := f.objs[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		for k, v := range o.meta {
			w.Header().Set("x-amz-meta-"+k, v)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(o.data)))
		w.Header().Set("Last-Modified", "Tue, 20 Oct 2026 10:00:00 GMT")
	case http.MethodPut:
		if src := r.Header.Get("x-amz-copy-source"); src != "" {
			_, srcKey, _ := strings.Cut(strings.TrimPrefix(src, "/"), "/")
			so, ok := f.objs[srcKey]
			if !ok {
				s3Err(w, 404, "NoSuchKey")
				return
			}
			if f.rejectCopy && srcKey == key {
				s3Err(w, 400, "InvalidRequest")
				return
			}
			nm := so.meta
			if strings.EqualFold(r.Header.Get("x-amz-metadata-directive"), "REPLACE") {
				nm = meta
			}
			f.objs[key] = &fakeObj{data: so.data, meta: nm}
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<CopyObjectResult><ETag>"x"</ETag></CopyObjectResult>`)
			return
		}
		if r.Header.Get("If-None-Match") == "*" {
			f.sawIfNone = true
			if _, ok := f.objs[key]; ok {
				s3Err(w, 412, "PreconditionFailed")
				return
			}
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(r.Header.Get("x-amz-content-sha256"), "STREAMING") {
			f.sawChecksum = true
		}
		f.objs[key] = &fakeObj{data: b, meta: meta}
	case http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(204)
	}
}

func newFakeStore(t *testing.T) (*S3Store, *fakeS3) {
	t.Helper()
	f := &fakeS3{objs: map[string]*fakeObj{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s, err := NewS3Store(Config{Bucket: "bkt", Endpoint: srv.URL, AccessKeyID: "k", SecretAccessKey: "s", Region: "auto", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

func TestS3StoreRoundTripAndErrors(t *testing.T) {
	ctx := context.Background()
	s, f := newFakeStore(t)
	body := []byte("PBDEMS2 bytes")
	meta := Meta{Kind: KindStudent, Patch: "60", Accounts: []string{"1", "2"}, Bad: true}.ToS3()
	if err := s.Put(ctx, "replays/1.dem", bytes.NewReader(body), int64(len(body)), PutOpts{Meta: meta, IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if !f.sawIfNone {
		t.Fatal("the conditional create did not send If-None-Match")
	}
	if err := s.Put(ctx, "replays/1.dem", bytes.NewReader(body), int64(len(body)), PutOpts{IfNoneMatch: true}); !errors.Is(err, ErrExists) {
		t.Fatalf("second conditional create = %v; want ErrExists", err)
	}
	if err := s.Put(ctx, "replays/1.dem", bytes.NewReader([]byte("v2")), 2, PutOpts{Meta: meta}); err != nil {
		t.Fatalf("plain put must overwrite: %v", err)
	}
	info, err := s.Head(ctx, "replays/1.dem")
	if err != nil || info.Size != 2 {
		t.Fatalf("head = %+v %v", info, err)
	}
	m := MetaFromS3(info.Meta)
	if m.Kind != KindStudent || m.Patch != "60" || len(m.Accounts) != 2 || !m.Bad {
		t.Fatalf("metadata did not survive S3: %+v (%v)", m, info.Meta)
	}
	if _, err := s.Head(ctx, "replays/none.dem"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("head missing = %v", err)
	}
	if _, _, err := s.Get(ctx, "replays/none.dem"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	rc, _, err := s.Get(ctx, "replays/1.dem")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "v2" {
		t.Fatalf("get = %q", got)
	}

	// metadata replace in place, bytes kept
	if err := s.CopyReplace(ctx, "replays/1.dem", Meta{Kind: KindPro, Patch: "59"}.ToS3()); err != nil {
		t.Fatal(err)
	}
	info, _ = s.Head(ctx, "replays/1.dem")
	if MetaFromS3(info.Meta).Kind != KindPro || info.Size != 2 {
		t.Fatalf("after replace = %+v", info)
	}
	// an endpoint that refuses a same-key copy goes through a temporary key
	f.rejectCopy = true
	if err := s.CopyReplace(ctx, "replays/1.dem", Meta{Kind: KindLadder}.ToS3()); err != nil {
		t.Fatalf("fallback copy: %v", err)
	}
	info, _ = s.Head(ctx, "replays/1.dem")
	if MetaFromS3(info.Meta).Kind != KindLadder || info.Size != 2 {
		t.Fatalf("after fallback = %+v", info)
	}
	if len(f.objs) != 1 {
		t.Fatalf("temporary key left behind: %v", f.objs)
	}
	if err := s.CopyReplace(ctx, "replays/none.dem", map[string]string{}); err == nil {
		t.Fatal("replace on a missing key must fail")
	}

	s.Put(ctx, "replays/2.dem", bytes.NewReader([]byte("x")), 1, PutOpts{})
	s.Put(ctx, "_vault/done/4.15.1/2", bytes.NewReader([]byte("{}")), 2, PutOpts{})
	var keys []string
	if err := s.List(ctx, "replays/", func(o ObjectInfo) error { keys = append(keys, fmt.Sprint(o.Key, ":", o.Size)); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "replays/1.dem:2,replays/2.dem:1" {
		t.Fatalf("list = %v", keys)
	}
	if err := s.Delete(ctx, "replays/2.dem"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Head(ctx, "replays/2.dem"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
	// through the Vault: PutNew conditional + merge over the real client
	v := New(s)
	if stored, err := v.PutNew(ctx, 7, bytes.NewReader(body), int64(len(body)), Meta{Kind: KindCoach}); err != nil || !stored {
		t.Fatalf("PutNew = %v %v", stored, err)
	}
	if stored, err := v.PutNew(ctx, 7, bytes.NewReader(body), int64(len(body)), Meta{Kind: KindStudent, Accounts: []string{"9"}}); err != nil || stored {
		t.Fatalf("PutNew again = %v %v", stored, err)
	}
	if o, _, _ := v.Find(ctx, 7); o.Meta.Kind != KindStudent || o.Meta.Accounts[0] != "9" {
		t.Fatalf("merged over S3 = %+v", o.Meta)
	}
}
