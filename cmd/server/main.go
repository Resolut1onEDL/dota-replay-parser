// parse-service: HTTP wrapper around the dota-replay-parser binary.
//
//	POST /parse        raw .dem or .dem.bz2 body            -> parsed JSON
//	POST /parse-valve  {"match_id":..,"cluster":..,"salt":..} -> download from Valve, parse
//	POST /parse-url    {"url":"https://.."}                   -> download .dem(.bz2) from URL, parse
//	GET  /healthz      -> {"ok":true,"parser":"<version>"}
//
// Env:
//
//	PARSE_TOKEN      bearer token required on POST endpoints (empty = no auth, dev only)
//	PARSER_BIN       path to the parser binary (default ./parser)
//	MAX_CONCURRENT   parallel parses (default 2)
//	PORT             listen port (default 8080)
package main

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

var (
	parserBin = envOr("PARSER_BIN", "./parser")
	// parserVersion is asked from the binary itself (--version, v4.6.0+);
	// older binaries have no flag and fall back to the basename so /healthz
	// still answers. Resolved once at startup.
	parserVersion   = detectParserVersion()
	token           = os.Getenv("PARSE_TOKEN")
	sem             chan struct{}
	inFlightMatches sync.Map
	// Replay download client with a total timeout. Valve's replay CDN is
	// wildly variable — some servers deliver a 150 MB .dem.bz2 at ~7 MB/s
	// (~20s), others at ~0.2 MB/s (12+ min). The default no-timeout client
	// waited out the slow ones, hanging the machine (and failing health
	// checks) for minutes. Cap it so a slow CDN fails fast and the caller
	// falls back, while a normal large download still completes.
	downloadClient = &http.Client{Timeout: downloadTimeout()}
	// How long a parsed answer may take to reach the client. A Fly suspend
	// mid-request leaves a connection the proxy has dropped and the frozen VM
	// never heard of; without a deadline the write waits out TCP retransmission
	// (~15 min, 2026-10-31) holding the parse slot. A few MB over Fly's network
	// take well under a second, so this only ever stops a dead client.
	responseWriteTimeout = 60 * time.Second
)

func downloadTimeout() time.Duration {
	if v := os.Getenv("DOWNLOAD_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 120 * time.Second
}

func detectParserVersion() string {
	out, err := exec.Command(envOr("PARSER_BIN", "./parser"), "--version").Output()
	if err == nil {
		if v := strings.TrimSpace(string(out)); v != "" && len(v) < 32 {
			return v
		}
	}
	return filepath.Base(envOr("PARSER_BIN", "./parser"))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func authed(r *http.Request) bool {
	if token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// writeDem writes the request body to a temp .dem file, transparently
// decompressing whatever container Valve sends.
//
// 2026-07-29: Valve switched new replays from bzip2 to ZSTD while keeping the
// .dem.bz2 URL — magic 28 B5 2F FD instead of BZh. Sniffing "BZh" only meant
// zstd bytes were written straight to the .dem file and every new match died
// with «unexpected magic: expected PBDEMS2» (service answered parse_failed,
// analyses silently fell back to OpenDota stats). Old bzip2 replays are still
// served for older matches, so BOTH must work — sniff, never trust the URL.
func writeDem(src io.Reader) (string, int64, error) {
	var track trackErr
	src = trackReader{src, &track}
	head := make([]byte, 4)
	n, err := io.ReadFull(src, head)
	if err != nil && n == 0 {
		return "", 0, fmt.Errorf("empty body")
	}
	// Буфер 4 МБ ОБЯЗАТЕЛЕН перед распаковкой: и zstd, и bzip2 читают источник
	// мелкими блоками, а источник здесь — сетевое тело ответа Valve. Замер на
	// матче 8918319751: zstd прямо с resp.Body — 2 мин 2 с (упирались в
	// 120-секундный таймаут клиента и отдавали 502 download_failed), тот же
	// поток через bufio 4 МБ — 2.3 с.
	var full io.Reader = bufio.NewReaderSize(io.MultiReader(bytes.NewReader(head[:n]), src), 4<<20)
	switch {
	case n >= 3 && string(head[:3]) == "BZh":
		full = bzip2.NewReader(full)
	case n >= 4 && head[0] == 0x28 && head[1] == 0xB5 && head[2] == 0x2F && head[3] == 0xFD:
		zr, zerr := zstd.NewReader(full)
		if zerr != nil {
			return "", 0, fmt.Errorf("zstd: %w", zerr)
		}
		defer zr.Close()
		full = zr.IOReadCloser()
	}

	f, err := os.CreateTemp("", "replay-*.dem")
	if err != nil {
		return "", 0, err
	}
	written, err := decodeInto(f, full, &track)
	closeErr := f.Close()
	if err != nil {
		os.Remove(f.Name())
		return "", 0, err
	}
	if closeErr != nil {
		os.Remove(f.Name())
		return "", 0, closeErr
	}
	return f.Name(), written, nil
}

// errDecode marks a replay whose bytes do not decode (a truncated or garbled
// bzip2/zstd stream). A failure to read the source or to write the temp file
// (a full disk) is not one: it says nothing about the replay.
var errDecode = errors.New("replay does not decode")

// trackErr remembers the first real I/O error seen on the source or the
// destination. The zstd decoder reads its source from its own goroutines, so
// it is locked.
type trackErr struct {
	mu  sync.Mutex
	err error
}

func (t *trackErr) note(err error) error {
	if err != nil && err != io.EOF {
		t.mu.Lock()
		if t.err == nil {
			t.err = err
		}
		t.mu.Unlock()
	}
	return err
}

func (t *trackErr) first() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

type trackReader struct {
	r io.Reader
	t *trackErr
}

func (r trackReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.t.note(err)
	return n, err
}

type trackWriter struct {
	w io.Writer
	t *trackErr
}

func (w trackWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.t.note(err)
	return n, err
}

// decodeInto copies the decoded stream to dst. An error that comes from the
// decoder itself is wrapped in errDecode; one that comes from reading src or
// writing dst is returned as it is.
func decodeInto(dst io.Writer, decoded io.Reader, t *trackErr) (int64, error) {
	n, err := io.Copy(trackWriter{dst, t}, decoded)
	if err != nil && t.first() == nil {
		err = fmt.Errorf("%w: %v", errDecode, err)
	}
	return n, err
}

func reserveSlot(w http.ResponseWriter, r *http.Request) bool {
	if r.Context().Err() != nil {
		http.Error(w, `{"error":"request_cancelled"}`, http.StatusRequestTimeout)
		return false
	}
	select {
	case sem <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, `{"error":"parser_busy"}`, http.StatusServiceUnavailable)
		return false
	}
}

func runParser(ctx context.Context, demPath string) ([]byte, time.Duration, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, parserBin, demPath)
	if vlt != nil {
		// with the vault on the child sees only what it needs (no vault, Fly or
		// GitHub secret); with it off the child inherits the env as it always did
		cmd.Env = parserEnv()
	}
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return out, time.Since(start), err
}

func handleParse(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if !reserveSlot(w, r) {
		return
	}
	defer func() { <-sem }()
	demPath, size, err := writeDem(http.MaxBytesReader(w, r.Body, 500<<20))
	if err != nil {
		http.Error(w, `{"error":"bad_body"}`, http.StatusBadRequest)
		return
	}
	defer os.Remove(demPath)
	log.Printf("parse: received %d MB", size>>20)
	serveParsed(r.Context(), w, demPath, 0)
}

type valveReq struct {
	MatchID int64 `json:"match_id"`
	Cluster int   `json:"cluster"`
	Salt    int64 `json:"salt"`
}

func handleParseValve(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req valveReq
	var rawBody bytes.Buffer
	body := io.Reader(r.Body)
	if vlt != nil {
		body = io.TeeReader(r.Body, &rawBody)
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil ||
		req.MatchID <= 0 || req.Cluster <= 0 || req.Salt <= 0 {
		http.Error(w, `{"error":"bad_params"}`, http.StatusBadRequest)
		return
	}
	var hints vaultHints
	if vlt != nil {
		var ok bool
		if hints, ok = parseVaultHints(r, rawBody.Bytes()); !ok {
			http.Error(w, `{"error":"bad_kind"}`, http.StatusBadRequest)
			return
		}
	}
	if _, loaded := inFlightMatches.LoadOrStore(req.MatchID, struct{}{}); loaded {
		w.Header().Set("Retry-After", "5")
		http.Error(w, `{"error":"parse_in_progress"}`, http.StatusServiceUnavailable)
		return
	}
	defer inFlightMatches.Delete(req.MatchID)
	if !reserveSlot(w, r) {
		return
	}
	defer func() { <-sem }()

	if vlt != nil {
		handleParseValveVault(w, r, req, hints)
		return
	}

	url := fmt.Sprintf("http://replay%d.valve.net/570/%d_%d.dem.bz2",
		req.Cluster, req.MatchID, req.Salt)
	log.Printf("parse-valve: downloading %s", url)

	dlStart := time.Now()
	dlReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	var resp *http.Response
	if err == nil {
		resp, err = downloadClient.Do(dlReq)
	}
	if err != nil {
		log.Printf("parse-valve: download error after %s: %v", time.Since(dlStart).Round(time.Millisecond), err)
		http.Error(w, `{"error":"valve_unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("parse-valve: valve returned %d", resp.StatusCode)
		http.Error(w,
			fmt.Sprintf(`{"error":"replay_unavailable","valve_status":%d}`, resp.StatusCode),
			http.StatusNotFound)
		return
	}

	demPath, size, err := writeDem(resp.Body)
	if err != nil {
		http.Error(w, `{"error":"download_failed"}`, http.StatusBadGateway)
		return
	}
	defer os.Remove(demPath)
	dl := time.Since(dlStart)
	log.Printf("parse-valve: downloaded+unpacked %d MB in %s", size>>20, dl.Round(time.Millisecond))
	serveParsed(r.Context(), w, demPath, dl)
}

type urlReq struct {
	URL string `json:"url"`
}

func handleParseURL(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req urlReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		!strings.HasPrefix(req.URL, "https://") {
		http.Error(w, `{"error":"bad_params"}`, http.StatusBadRequest)
		return
	}
	if !reserveSlot(w, r) {
		return
	}
	defer func() { <-sem }()

	// signed URLs carry auth in the query string — keep it out of logs
	logURL := req.URL
	if i := strings.IndexByte(logURL, '?'); i >= 0 {
		logURL = logURL[:i]
	}
	log.Printf("parse-url: downloading %s", logURL)

	dlStart := time.Now()
	dlReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, req.URL, nil)
	var resp *http.Response
	if err == nil {
		resp, err = downloadClient.Do(dlReq)
	}
	if err != nil {
		log.Printf("parse-url: download error after %s: %v", time.Since(dlStart).Round(time.Millisecond), err)
		http.Error(w, `{"error":"source_unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("parse-url: source returned %d", resp.StatusCode)
		http.Error(w,
			fmt.Sprintf(`{"error":"source_unavailable","source_status":%d}`, resp.StatusCode),
			http.StatusNotFound)
		return
	}

	demPath, size, err := writeDem(io.LimitReader(resp.Body, 500<<20))
	if err != nil {
		http.Error(w, `{"error":"download_failed"}`, http.StatusBadGateway)
		return
	}
	defer os.Remove(demPath)
	dl := time.Since(dlStart)
	log.Printf("parse-url: downloaded+unpacked %d MB in %s", size>>20, dl.Round(time.Millisecond))
	serveParsed(r.Context(), w, demPath, dl)
}

func serveParsed(ctx context.Context, w http.ResponseWriter, demPath string, download time.Duration) {
	out, parseDur, err := runParser(ctx, demPath)
	if err != nil {
		log.Printf("parser failed: %v", err)
		http.Error(w, `{"error":"parse_failed"}`, http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if download > 0 {
		w.Header().Set("X-Download-Ms", fmt.Sprint(download.Milliseconds()))
	}
	w.Header().Set("X-Parse-Ms", fmt.Sprint(parseDur.Milliseconds()))
	log.Printf("parsed in %s (json %d KB)", parseDur.Round(time.Millisecond), len(out)>>10)
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	w.Write(out)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"parser":%q}`, parserVersion)
}

func main() {
	if role := startRole(os.Args[1:], os.Getenv); role != "" {
		os.Exit(runRole(role))
	}
	initVault()
	maxConc := 2
	if v := os.Getenv("MAX_CONCURRENT"); v != "" {
		fmt.Sscanf(v, "%d", &maxConc)
	}
	sem = make(chan struct{}, maxConc)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("POST /parse", handleParse)
	mux.HandleFunc("POST /parse-valve", handleParseValve)
	mux.HandleFunc("POST /parse-url", handleParseURL)
	if vlt != nil {
		registerVaultRoutes(mux)
	}

	port := envOr("PORT", "8080")
	log.Printf("parse-service listening on :%s (parser=%s, max_concurrent=%d, auth=%v)",
		port, parserBin, maxConc, token != "")
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
