// audit: compares the parser with the public statistics of fresh matches (audit package).
//
//	audit pick  -dir D [-n 100] [-pro 0.5]   choose matches: league games + ranked pubs of the current patch,
//	                                           every hero at least twice where the pool allows → D/matches.json
//	audit fetch -dir D                         OpenDota's answer for each (a parse is requested for unparsed pubs)
//	                                           and the replay from Valve → D/<id>_opendota.json, D/<id>.dem.bz2
//	audit run   -dir D -parser P [-baseline B] parse each replay with P, compare, write D/report.md + report.json;
//	            [-write-baseline B]            exits 1 when a field matches worse than the baseline allows
//
// OpenDota is asked politely (one request a second, OPENDOTA_API_KEY appended when set); replays come from
// Valve's CDN as OpenDota names them.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dota-replay-parser/audit"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: audit pick|fetch|run -dir <dir> …")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "pick":
		err = pick(os.Args[2:])
	case "fetch":
		err = fetch(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if errors.Is(err, errRegression) {
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit:", err)
		os.Exit(2)
	}
}

var errRegression = errors.New("regression")

// ---- OpenDota ---------------------------------------------------------------------------------

type odClient struct {
	mu   sync.Mutex
	last time.Time
	http *http.Client
}

var od = &odClient{http: &http.Client{Timeout: 60 * time.Second}}

func (c *odClient) do(method, path string, out interface{}) error {
	url := "https://api.opendota.com/api" + path
	if key := os.Getenv("OPENDOTA_API_KEY"); key != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		url += sep + "api_key=" + key
	}
	backoff := []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second}
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		if wait := time.Until(c.last.Add(1100 * time.Millisecond)); wait > 0 {
			time.Sleep(wait)
		}
		c.last = time.Now()
		c.mu.Unlock()
		req, _ := http.NewRequest(method, url, nil)
		req.Header.Set("User-Agent", "dota-replay-parser-audit/1.0")
		res, err := c.http.Do(req)
		if err == nil && res.StatusCode < 300 {
			defer res.Body.Close()
			if out == nil {
				_, _ = io.Copy(io.Discard, res.Body)
				return nil
			}
			return json.NewDecoder(res.Body).Decode(out)
		}
		status := 0
		if res != nil {
			status = res.StatusCode
			res.Body.Close()
		}
		retry := err != nil || status == 429 || status >= 500
		if !retry || attempt >= len(backoff) {
			if err != nil {
				return fmt.Errorf("%s %s: %w", method, path, err)
			}
			return fmt.Errorf("%s %s: HTTP %d", method, path, status)
		}
		time.Sleep(backoff[attempt])
	}
}

func (c *odClient) get(path string, out interface{}) error { return c.do("GET", path, out) }

// getMatch fetches /matches/{id}, keeps OpenDota's raw answer in the audit directory and decodes it.
func (c *odClient) getMatch(dir string, id int64, out *audit.ODMatch) error {
	var raw json.RawMessage
	if err := c.get(fmt.Sprintf("/matches/%d", id), &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	return writeFile(odPath(dir, id), raw)
}

// ---- pick -------------------------------------------------------------------------------------

type picked struct {
	MatchID int64  `json:"match_id"`
	Kind    string `json:"kind"` // "pro" | "pub"
	Heroes  []int  `json:"heroes"`
}

func pick(args []string) error {
	fs := flag.NewFlagSet("pick", flag.ExitOnError)
	dir := fs.String("dir", "", "audit directory")
	n := fs.Int("n", 100, "matches to pick")
	proShare := fs.Float64("pro", 0.5, "share of league games")
	minRank := fs.Int("min-rank", 50, "lowest average rank tier of a pub (50 = Legend)")
	minDur := fs.Int("min-duration", 900, "shortest game, seconds")
	_ = fs.Parse(args)
	if *dir == "" {
		return errors.New("-dir is required")
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	var patches []struct {
		ID int `json:"id"`
	}
	if err := od.get("/constants/patch", &patches); err != nil || len(patches) == 0 {
		return fmt.Errorf("current patch: %v", err)
	}
	patch := patches[len(patches)-1].ID
	nPro := int(float64(*n)*(*proShare) + 0.5)
	nPub := *n - nPro
	count := map[int]int{}

	// league games: OpenDota parses them itself and Valve keeps their replays for months
	var proPool []picked
	var less int64
	for page := 0; page < 10 && len(proPool) < 3*nPro; page++ {
		var list []struct {
			MatchID  int64 `json:"match_id"`
			Duration int   `json:"duration"`
			Version  *int  `json:"version"`
		}
		path := "/proMatches"
		if less > 0 {
			path += "?less_than_match_id=" + strconv.FormatInt(less, 10)
		}
		if err := od.get(path, &list); err != nil || len(list) == 0 {
			break
		}
		for _, m := range list {
			less = m.MatchID
			if m.Version == nil || m.Duration < *minDur {
				continue
			}
			var full audit.ODMatch
			if err := od.getMatch(*dir, m.MatchID, &full); err != nil {
				continue
			}
			if full.Patch != patch || full.ReplayURL == "" || len(full.Players) != 10 {
				os.Remove(odPath(*dir, m.MatchID))
				continue
			}
			p := picked{MatchID: m.MatchID, Kind: "pro"}
			for _, pl := range full.Players {
				p.Heroes = append(p.Heroes, pl.HeroID)
			}
			proPool = append(proPool, p)
			if len(proPool) >= 3*nPro {
				break
			}
		}
	}
	out := greedy(proPool, nPro, count)

	// ranked pubs: the public feed, ranked All Pick only; replays live ~14 days, so only fresh ones
	var pubPool []picked
	less = 0
	for page := 0; page < 60 && len(pubPool) < 6*nPub; page++ {
		var list []struct {
			MatchID     int64 `json:"match_id"`
			Duration    int   `json:"duration"`
			LobbyType   int   `json:"lobby_type"`
			GameMode    int   `json:"game_mode"`
			RadiantTeam []int `json:"radiant_team"`
			DireTeam    []int `json:"dire_team"`
		}
		path := fmt.Sprintf("/publicMatches?min_rank=%d", *minRank)
		if less > 0 {
			path += "&less_than_match_id=" + strconv.FormatInt(less, 10)
		}
		if err := od.get(path, &list); err != nil || len(list) == 0 {
			break
		}
		for _, m := range list {
			if less == 0 || m.MatchID < less {
				less = m.MatchID
			}
			heroes := append(append([]int{}, m.RadiantTeam...), m.DireTeam...)
			if m.LobbyType != 7 || m.GameMode != 22 || m.Duration < *minDur || len(heroes) != 10 || contains(heroes, 0) {
				continue
			}
			pubPool = append(pubPool, picked{MatchID: m.MatchID, Kind: "pub", Heroes: heroes})
		}
	}
	out = append(out, greedy(pubPool, nPub, count)...)
	covered, twice := 0, 0
	for _, c := range count {
		covered++
		if c >= 2 {
			twice++
		}
	}
	fmt.Fprintf(os.Stderr, "picked %d matches (patch %d): %d heroes seen, %d at least twice\n", len(out), patch, covered, twice)
	return writeJSON(filepath.Join(*dir, "matches.json"), out)
}

// greedy takes, n times, the match that adds the most heroes still seen fewer than twice.
func greedy(pool []picked, n int, count map[int]int) []picked {
	var out []picked
	used := make([]bool, len(pool))
	for len(out) < n {
		best, bestScore := -1, -1
		for i, p := range pool {
			if used[i] {
				continue
			}
			score := 0
			for _, h := range p.Heroes {
				if count[h] < 2 {
					score++
				}
			}
			if score > bestScore {
				best, bestScore = i, score
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		for _, h := range pool[best].Heroes {
			count[h]++
		}
		out = append(out, pool[best])
	}
	return out
}

// ---- fetch ------------------------------------------------------------------------------------

func fetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	dir := fs.String("dir", "", "audit directory")
	wait := fs.Duration("wait", 20*time.Minute, "how long to wait for OpenDota to parse the requested pubs")
	parallel := fs.Int("parallel", 3, "parallel replay downloads")
	_ = fs.Parse(args)
	list, err := readPicked(*dir)
	if err != nil {
		return err
	}
	// 1. OpenDota's answer; a parse is requested for each pub it has not parsed yet
	var pending []int64
	for _, m := range list {
		file := odPath(*dir, m.MatchID)
		var cur audit.ODMatch
		if readJSON(file, &cur) == nil && cur.Version != nil {
			continue
		}
		if err := od.getMatch(*dir, m.MatchID, &cur); err != nil {
			fmt.Fprintf(os.Stderr, "%d: OpenDota: %v\n", m.MatchID, err)
			continue
		}
		if cur.Version == nil {
			if err := od.do("POST", fmt.Sprintf("/request/%d", m.MatchID), nil); err != nil {
				fmt.Fprintf(os.Stderr, "%d: parse request: %v\n", m.MatchID, err)
			}
			pending = append(pending, m.MatchID)
		}
	}
	// 2. replays from Valve, while OpenDota parses
	jobs := make(chan picked)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	for w := 0; w < *parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range jobs {
				err := download(*dir, m.MatchID)
				if err != nil {
					// Valve's CDN drops a download now and then (a cut body, a timeout); one more try
					time.Sleep(10 * time.Second)
					err = download(*dir, m.MatchID)
				}
				if err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "%d: replay: %v\n", m.MatchID, err)
				}
			}
		}()
	}
	for _, m := range list {
		jobs <- m
	}
	close(jobs)
	wg.Wait()
	// 3. the requested parses
	deadline := time.Now().Add(*wait)
	for len(pending) > 0 && time.Now().Before(deadline) {
		var still []int64
		for _, id := range pending {
			var cur audit.ODMatch
			if err := od.getMatch(*dir, id, &cur); err == nil && cur.Version != nil {
				continue
			}
			still = append(still, id)
		}
		pending = still
		if len(pending) > 0 {
			time.Sleep(30 * time.Second)
		}
	}
	fmt.Fprintf(os.Stderr, "fetched %d matches: %d replays failed, %d still unparsed by OpenDota (Valve's numbers only)\n", len(list), failed, len(pending))
	return nil
}

func download(dir string, id int64) error {
	dst := filepath.Join(dir, fmt.Sprintf("%d.dem.bz2", id))
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return nil
	}
	var m audit.ODMatch
	if err := readJSON(odPath(dir, id), &m); err != nil {
		return err
	}
	url := m.ReplayURL
	if url == "" && m.Cluster > 0 && m.ReplaySalt > 0 {
		url = fmt.Sprintf("http://replay%d.valve.net/570/%d_%d.dem.bz2", m.Cluster, id, m.ReplaySalt)
	}
	if url == "" {
		return errors.New("no replay url")
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	res, err := client.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, res.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ---- run --------------------------------------------------------------------------------------

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := fs.String("dir", "", "audit directory")
	parser := fs.String("parser", "./parser", "parser binary")
	baseline := fs.String("baseline", "", "fail when a field matches worse than this baseline")
	writeBase := fs.String("write-baseline", "", "write today's rates as the baseline")
	title := fs.String("title", "Parser audit", "report title")
	_ = fs.Parse(args)
	list, err := readPicked(*dir)
	if err != nil {
		return err
	}
	verOut, err := exec.Command(*parser, "--version").Output()
	if err != nil {
		return fmt.Errorf("parser --version: %w", err)
	}
	version := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(verOut)), "dota-replay-parser"))
	// parses are cached per binary, not per version: a rebuilt binary of the same version parses again
	sum, err := fileHash(*parser)
	if err != nil {
		return err
	}
	outDir := filepath.Join(*dir, "out", version+"-"+sum)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	consts := loadConsts(*dir)

	var checks []audit.Check
	compared, parsedByOD := 0, 0
	for _, m := range list {
		dem := filepath.Join(*dir, fmt.Sprintf("%d.dem.bz2", m.MatchID))
		var odm audit.ODMatch
		if readJSON(odPath(*dir, m.MatchID), &odm) != nil {
			continue
		}
		if _, err := os.Stat(dem); err != nil {
			continue
		}
		outFile := filepath.Join(outDir, fmt.Sprintf("%d.json", m.MatchID))
		if _, err := os.Stat(outFile); err != nil {
			if err := parse(*parser, dem, outFile); err != nil {
				fmt.Fprintf(os.Stderr, "%d: parse: %v\n", m.MatchID, err)
				continue
			}
		}
		var ours audit.Match
		if err := readJSON(outFile, &ours); err != nil {
			fmt.Fprintf(os.Stderr, "%d: parser output: %v\n", m.MatchID, err)
			continue
		}
		compared++
		if odm.Version != nil {
			parsedByOD++
		}
		checks = append(checks, audit.Compare(&ours, &odm, consts)...)
	}
	if compared == 0 {
		return errors.New("nothing to compare: no replay with an OpenDota answer")
	}
	stats := audit.Aggregate(checks)
	var regressions []string
	if *baseline != "" {
		b, err := audit.LoadBaseline(*baseline)
		if err != nil {
			return fmt.Errorf("baseline: %w", err)
		}
		regressions = audit.Regressions(stats, b)
		if regressions == nil {
			regressions = []string{}
		}
	}
	md := audit.Markdown(fmt.Sprintf("%s — parser %s", *title, version), compared, parsedByOD, stats, regressions)
	if err := os.WriteFile(filepath.Join(*dir, "report.md"), []byte(md), 0o644); err != nil {
		return err
	}
	writeJSON(filepath.Join(*dir, "report.json"), map[string]interface{}{"parser": version, "matches": compared, "parsedByOpenDota": parsedByOD, "fields": stats, "regressions": regressions})
	writeJSON(filepath.Join(*dir, "checks-"+version+".json"), checks)
	if *writeBase != "" {
		writeJSON(*writeBase, audit.NewBaseline(stats))
	}
	fmt.Printf("%d matches (%d parsed by OpenDota), %d fields → %s\n", compared, parsedByOD, len(stats), filepath.Join(*dir, "report.md"))
	if len(regressions) > 0 {
		for _, r := range regressions {
			fmt.Println("worse than baseline:", r)
		}
		return errRegression
	}
	return nil
}

func parse(bin, dem, outFile string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	tmp := outFile + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, dem)
	cmd.Stdout = f
	cmd.Stderr = io.Discard
	err = cmd.Run()
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, outFile)
}

// loadConsts reads OpenDota's ability ids and hero names, cached in the audit directory.
func loadConsts(dir string) audit.Consts {
	c := audit.Consts{Abilities: map[int]string{}, Heroes: map[string]int{}}
	var ab map[string]string
	if cached(filepath.Join(dir, "consts", "ability_ids.json"), "/constants/ability_ids", &ab) == nil {
		for k, v := range ab {
			if id, err := strconv.Atoi(k); err == nil {
				c.Abilities[id] = v
			}
		}
	}
	var heroes map[string]struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if cached(filepath.Join(dir, "consts", "heroes.json"), "/constants/heroes", &heroes) == nil {
		for _, h := range heroes {
			c.Heroes[h.Name] = h.ID
		}
	}
	return c
}

func cached(file, path string, out interface{}) error {
	if readJSON(file, out) == nil {
		return nil
	}
	if err := od.get(path, out); err != nil {
		fmt.Fprintf(os.Stderr, "OpenDota %s: %v\n", path, err)
		return err
	}
	_ = os.MkdirAll(filepath.Dir(file), 0o755)
	return writeJSON(file, out)
}

// ---- files ------------------------------------------------------------------------------------

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:8], nil
}

func odPath(dir string, id int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d_opendota.json", id))
}

func readPicked(dir string) ([]picked, error) {
	if dir == "" {
		return nil, errors.New("-dir is required")
	}
	var list []picked
	if err := readJSON(filepath.Join(dir, "matches.json"), &list); err != nil {
		return nil, fmt.Errorf("matches.json: %w (run `audit pick` first)", err)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].MatchID < list[j].MatchID })
	return list, nil
}

func readJSON(path string, out interface{}) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func writeJSON(path string, v interface{}) error {
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return writeFile(path, raw)
}

func writeFile(path string, raw []byte) error {
	tmp := path + ".part"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
