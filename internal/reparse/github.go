package reparse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHub is the seam for the one dispatch at the end of a run.
type GitHub interface {
	// RunsWaiting: a run of the workflow is already queued, waiting or pending.
	RunsWaiting(ctx context.Context) (bool, error)
	Dispatch(ctx context.Context) error
}

// HTTPGitHub calls the GitHub REST API with a fine-grained PAT (GH_DISPATCH_TOKEN).
type HTTPGitHub struct {
	Base     string // default https://api.github.com
	Repo     string // default Resolut1onEDL/ResoAI-web
	Workflow string // default student-refresh.yml
	Ref      string // default main
	Token    string // never logged
	Client   *http.Client
}

func (g *HTTPGitHub) fill() (base, repo, wf, ref string, c *http.Client) {
	base, repo, wf, ref, c = strings.TrimRight(g.Base, "/"), g.Repo, g.Workflow, g.Ref, g.Client
	if base == "" {
		base = "https://api.github.com"
	}
	if repo == "" {
		repo = "Resolut1onEDL/ResoAI-web"
	}
	if wf == "" {
		wf = "student-refresh.yml"
	}
	if ref == "" {
		ref = "main"
	}
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return
}

func (g *HTTPGitHub) req(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	_, _, _, _, c := g.fill()
	r, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+g.Token)
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return c.Do(r)
}

func (g *HTTPGitHub) RunsWaiting(ctx context.Context) (bool, error) {
	base, repo, wf, ref, _ := g.fill()
	for _, status := range []string{"queued", "waiting", "pending"} {
		resp, err := g.req(ctx, http.MethodGet,
			fmt.Sprintf("%s/repos/%s/actions/workflows/%s/runs?status=%s&branch=%s&per_page=1", base, repo, wf, status, ref), nil)
		if err != nil {
			return false, err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("github runs: %d", resp.StatusCode)
		}
		var out struct {
			Total int `json:"total_count"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return false, err
		}
		if out.Total > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (g *HTTPGitHub) Dispatch(ctx context.Context) error {
	base, repo, wf, ref, _ := g.fill()
	body, _ := json.Marshal(map[string]string{"ref": ref})
	resp, err := g.req(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/actions/workflows/%s/dispatches", base, repo, wf), body)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("github dispatch: %d", resp.StatusCode)
	}
	return nil
}
