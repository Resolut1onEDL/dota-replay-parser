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
	"bytes"
	"compress/bzip2"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

var (
	parserBin = envOr("PARSER_BIN", "./parser")
	token     = os.Getenv("PARSE_TOKEN")
	sem       chan struct{}
	// Replay download client with a total timeout. Valve's replay CDN is
	// wildly variable — some servers deliver a 150 MB .dem.bz2 at ~7 MB/s
	// (~20s), others at ~0.2 MB/s (12+ min). The default no-timeout client
	// waited out the slow ones, hanging the machine (and failing health
	// checks) for minutes. Cap it so a slow CDN fails fast and the caller
	// falls back, while a normal large download still completes.
	downloadClient = &http.Client{Timeout: downloadTimeout()}
)

func downloadTimeout() time.Duration {
	if v := os.Getenv("DOWNLOAD_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 120 * time.Second
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
	head := make([]byte, 4)
	n, err := io.ReadFull(src, head)
	if err != nil && n == 0 {
		return "", 0, fmt.Errorf("empty body")
	}
	full := io.MultiReader(bytes.NewReader(head[:n]), src)
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
	written, err := io.Copy(f, full)
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

func runParser(demPath string) ([]byte, time.Duration, error) {
	sem <- struct{}{}
	defer func() { <-sem }()

	start := time.Now()
	cmd := exec.Command(parserBin, demPath)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return out, time.Since(start), err
}

func handleParse(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	demPath, size, err := writeDem(http.MaxBytesReader(w, r.Body, 500<<20))
	if err != nil {
		http.Error(w, `{"error":"bad_body"}`, http.StatusBadRequest)
		return
	}
	defer os.Remove(demPath)
	log.Printf("parse: received %d MB", size>>20)
	serveParsed(w, demPath, 0)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.MatchID <= 0 || req.Cluster <= 0 || req.Salt <= 0 {
		http.Error(w, `{"error":"bad_params"}`, http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("http://replay%d.valve.net/570/%d_%d.dem.bz2",
		req.Cluster, req.MatchID, req.Salt)
	log.Printf("parse-valve: downloading %s", url)

	dlStart := time.Now()
	resp, err := downloadClient.Get(url)
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
	serveParsed(w, demPath, dl)
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

	// signed URLs carry auth in the query string — keep it out of logs
	logURL := req.URL
	if i := strings.IndexByte(logURL, '?'); i >= 0 {
		logURL = logURL[:i]
	}
	log.Printf("parse-url: downloading %s", logURL)

	dlStart := time.Now()
	resp, err := downloadClient.Get(req.URL)
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
	serveParsed(w, demPath, dl)
}

func serveParsed(w http.ResponseWriter, demPath string, download time.Duration) {
	out, parseDur, err := runParser(demPath)
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
	w.Write(out)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"parser":%q}`, filepath.Base(parserBin))
}

func main() {
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

	port := envOr("PORT", "8080")
	log.Printf("parse-service listening on :%s (parser=%s, max_concurrent=%d, auth=%v)",
		port, parserBin, maxConc, token != "")
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
