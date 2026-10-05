package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lifecycleWait only guards against a hung test: passing runs never wait it
// out. The fakes block for far longer (the parser sleeps 30s, downloads wait
// for cleanup), so finishing inside it still proves the cancel stopped them.
// A 1s start / 250ms stop budget failed under CPU load (macOS took >1s to
// start the fake parser script) with the server behaving correctly.
const lifecycleWait = 10 * time.Second

type lifecycleTransport func(*http.Request) (*http.Response, error)

func (f lifecycleTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type lifecycleBody struct{ reads int }

func (b *lifecycleBody) Read([]byte) (int, error) {
	b.reads++
	return 0, io.EOF
}

func lifecycleSetup(t *testing.T, slots int) {
	t.Helper()
	oldSem, oldClient, oldBin, oldToken := sem, downloadClient, parserBin, token
	sem, token = make(chan struct{}, slots), ""
	t.Cleanup(func() { sem, downloadClient, parserBin, token = oldSem, oldClient, oldBin, oldToken })
	parserBin = filepath.Join(t.TempDir(), "parser")
	if err := os.WriteFile(parserBin, []byte("#!/bin/sh\nprintf '{\"match_id\":123}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

var lifecycleEndpoints = []struct {
	name, body string
	handler    http.HandlerFunc
}{
	{"parse", "PBDEMS2\x00replay", handleParse},
	{"parse-valve", `{"match_id":123,"cluster":1,"salt":456}`, handleParseValve},
	{"parse-url", `{"url":"https://example.com/replay.dem"}`, handleParseURL},
}

func TestBusyRejectsBeforeReceivingReplay(t *testing.T) {
	for _, endpoint := range lifecycleEndpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			lifecycleSetup(t, 1)
			sem <- struct{}{}
			downloads := 0
			downloadClient = &http.Client{Transport: lifecycleTransport(func(*http.Request) (*http.Response, error) {
				downloads++
				return nil, errors.New("download must not start when busy")
			})}
			body := &lifecycleBody{}
			req := httptest.NewRequest(http.MethodPost, "/"+endpoint.name, strings.NewReader(endpoint.body))
			if endpoint.name == "parse" {
				req.Body = io.NopCloser(body)
			}
			w := httptest.NewRecorder()
			endpoint.handler(w, req)
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
				t.Errorf("busy response = %d, Retry-After = %q; want 503 with retry guidance", w.Code, w.Header().Get("Retry-After"))
			}
			if downloads != 0 || body.reads != 0 {
				t.Errorf("busy request consumed replay: downloads=%d, body reads=%d", downloads, body.reads)
			}
		})
	}
}

func TestConcurrentMatchIsNotDownloadedTwice(t *testing.T) {
	lifecycleSetup(t, 2)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var downloads atomic.Int32
	downloadClient = &http.Client{Transport: lifecycleTransport(func(*http.Request) (*http.Response, error) {
		if downloads.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil, errors.New("test download unavailable")
	})}
	body := `{"match_id":123,"cluster":1,"salt":456}`
	go func() {
		defer close(done)
		handleParseValve(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/parse-valve", strings.NewReader(body)))
	}()
	t.Cleanup(func() { close(release); <-done })
	select {
	case <-started:
	case <-time.After(lifecycleWait):
		t.Fatal("first download did not start")
	}
	w := httptest.NewRecorder()
	handleParseValve(w, httptest.NewRequest(http.MethodPost, "/parse-valve", strings.NewReader(body)))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || downloads.Load() != 1 {
		t.Fatalf("duplicate response=%d, retry=%q, downloads=%d; want 503 and only the original download", w.Code, w.Header().Get("Retry-After"), downloads.Load())
	}
}

func TestDownloadStopsWhenRequestIsCancelled(t *testing.T) {
	for _, endpoint := range lifecycleEndpoints[1:] {
		t.Run(endpoint.name, func(t *testing.T) {
			lifecycleSetup(t, 1)
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			downloadClient = &http.Client{Transport: lifecycleTransport(func(r *http.Request) (*http.Response, error) {
				close(started)
				select {
				case <-r.Context().Done():
					return nil, r.Context().Err()
				case <-release:
					return nil, errors.New("test cleanup")
				}
			})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				defer close(done)
				endpoint.handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/"+endpoint.name, strings.NewReader(endpoint.body)).WithContext(ctx))
			}()
			t.Cleanup(func() { close(release); <-done })
			select {
			case <-started:
			case <-time.After(lifecycleWait):
				t.Fatal("download did not start")
			}
			cancel()
			select {
			case <-done:
				if len(sem) != 0 {
					t.Fatal("cancelled download retained its slot")
				}
			case <-time.After(lifecycleWait):
				t.Fatal("download kept running after the client cancelled")
			}
		})
	}
}

func TestParserStopsWhenRequestIsCancelled(t *testing.T) {
	lifecycleSetup(t, 1)
	marker := filepath.Join(t.TempDir(), "pid")
	if err := os.WriteFile(parserBin, []byte(fmt.Sprintf("#!/bin/sh\necho \"$1\" > %q\necho $$ > %q\nexec sleep 30\n", marker+".dempath", marker)), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleParse(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/parse", strings.NewReader("PBDEMS2\x00replay")).WithContext(ctx))
	}()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		if data, err := os.ReadFile(marker); err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if process, err := os.FindProcess(pid); err == nil {
				_ = process.Kill()
			}
		}
		<-done
	})
	deadline := time.Now().Add(lifecycleWait)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatal("handler returned before the parser process started")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("parser process did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
		if len(sem) != 0 {
			t.Fatal("cancelled parser retained its slot")
		}
		data, err := os.ReadFile(marker + ".dempath")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(strings.TrimSpace(string(data))); !os.IsNotExist(err) {
			t.Fatalf("cancelled parser left its temporary replay: %v", err)
		}
	case <-time.After(lifecycleWait):
		t.Fatal("parser kept running after the client cancelled")
	}
}

func TestDownloadHonorsRequestDeadline(t *testing.T) {
	for _, endpoint := range lifecycleEndpoints[1:] {
		t.Run(endpoint.name, func(t *testing.T) {
			lifecycleSetup(t, 1)
			downloadClient = &http.Client{Transport: lifecycleTransport(func(r *http.Request) (*http.Response, error) {
				if _, ok := r.Context().Deadline(); !ok {
					return nil, errors.New("download did not inherit the request deadline")
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			w := httptest.NewRecorder()
			endpoint.handler(w, httptest.NewRequest(http.MethodPost, "/"+endpoint.name, strings.NewReader(endpoint.body)).WithContext(ctx))
			if ctx.Err() != context.DeadlineExceeded || len(sem) != 0 {
				t.Fatalf("download did not end with the request deadline: context=%v, slots=%d", ctx.Err(), len(sem))
			}
			if _, active := inFlightMatches.Load(int64(123)); active {
				t.Fatal("deadline retained the match lock")
			}
		})
	}
}

func TestSuccessfulResponsesAndReleasedSlots(t *testing.T) {
	for _, endpoint := range lifecycleEndpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			lifecycleSetup(t, 1)
			downloadClient = &http.Client{Transport: lifecycleTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("PBDEMS2\x00replay"))}, nil
			})}
			for attempt := 0; attempt < 2; attempt++ {
				w := httptest.NewRecorder()
				endpoint.handler(w, httptest.NewRequest(http.MethodPost, "/"+endpoint.name, strings.NewReader(endpoint.body)))
				if w.Code != http.StatusOK || w.Body.String() != `{"match_id":123}` || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("X-Parse-Ms") == "" {
					t.Fatalf("success contract changed: status=%d, body=%q, headers=%v", w.Code, w.Body.String(), w.Header())
				}
				if len(sem) != 0 {
					t.Fatal("completed request retained its slot")
				}
			}
		})
	}
}

func TestValveFailureAllowsRetry(t *testing.T) {
	lifecycleSetup(t, 1)
	calls := 0
	downloadClient = &http.Client{Transport: lifecycleTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not ready"))}, nil
	})}
	for attempt := 0; attempt < 2; attempt++ {
		w := httptest.NewRecorder()
		handleParseValve(w, httptest.NewRequest(http.MethodPost, "/parse-valve", strings.NewReader(`{"match_id":123,"cluster":1,"salt":456}`)))
		if w.Code != http.StatusNotFound || len(sem) != 0 {
			t.Fatalf("failed request was not released: status=%d, occupied slots=%d", w.Code, len(sem))
		}
	}
	if calls != 2 {
		t.Fatalf("retry was not downloaded: calls=%d", calls)
	}
}
