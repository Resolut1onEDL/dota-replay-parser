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

// Guest, Restart and MachineConfig are the parts of the Fly Machines config
// the re-parse uses.
type Guest struct {
	CPUKind  string `json:"cpu_kind"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
}

type Restart struct {
	Policy string `json:"policy"`
}

type MachineConfig struct {
	Image       string            `json:"image"`
	Env         map[string]string `json:"env,omitempty"`
	Guest       *Guest            `json:"guest,omitempty"`
	Restart     *Restart          `json:"restart,omitempty"`
	AutoDestroy bool              `json:"auto_destroy,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Schedule    string            `json:"schedule,omitempty"`
}

// CreateRequest is the body of create and update calls.
type CreateRequest struct {
	Name       string        `json:"name,omitempty"`
	Region     string        `json:"region,omitempty"`
	SkipLaunch bool          `json:"skip_launch,omitempty"`
	Config     MachineConfig `json:"config"`
}

// Machine is the part of a listed machine the planner reads.
type Machine struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	State     string        `json:"state"`
	CreatedAt string        `json:"created_at"`
	Config    MachineConfig `json:"config"`
}

// Alive: a machine that is, or is about to be, running.
func (m Machine) Alive() bool {
	switch m.State {
	case "created", "starting", "started", "replacing":
		return true
	}
	return false
}

func (m Machine) Created() time.Time {
	t, _ := time.Parse(time.RFC3339, m.CreatedAt)
	return t
}

// MachinesAPI is the Fly Machines seam (HTTPMachines in production, a fake in tests).
type MachinesAPI interface {
	Create(ctx context.Context, req CreateRequest) (Machine, error)
	List(ctx context.Context) ([]Machine, error)
	Update(ctx context.Context, id string, req CreateRequest) error
	Delete(ctx context.Context, id string) error
}

// HTTPMachines talks to https://api.machines.dev.
type HTTPMachines struct {
	Base   string // default https://api.machines.dev
	App    string
	Token  string // never logged
	Client *http.Client
	// RetryDelay is the pause before a retry (429 / 5xx); tests shorten it.
	RetryDelay time.Duration
}

func (h *HTTPMachines) do(ctx context.Context, method, path string, body any, out any) error {
	base := strings.TrimRight(h.Base, "/")
	if base == "" {
		base = "https://api.machines.dev"
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	delay := h.RetryDelay
	if delay == 0 {
		delay = 2 * time.Second
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay * time.Duration(attempt)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+h.Token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("machines api %s %s: %d", method, path, resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("machines api %s %s: %d %s", method, path, resp.StatusCode, truncate(string(b), 200))
		}
		if out != nil && len(b) > 0 {
			return json.Unmarshal(b, out)
		}
		return nil
	}
	return lastErr
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (h *HTTPMachines) Create(ctx context.Context, req CreateRequest) (Machine, error) {
	var m Machine
	err := h.do(ctx, http.MethodPost, "/v1/apps/"+h.App+"/machines", req, &m)
	return m, err
}

func (h *HTTPMachines) List(ctx context.Context) ([]Machine, error) {
	var ms []Machine
	err := h.do(ctx, http.MethodGet, "/v1/apps/"+h.App+"/machines", nil, &ms)
	return ms, err
}

func (h *HTTPMachines) Update(ctx context.Context, id string, req CreateRequest) error {
	return h.do(ctx, http.MethodPost, "/v1/apps/"+h.App+"/machines/"+id, req, nil)
}

func (h *HTTPMachines) Delete(ctx context.Context, id string) error {
	return h.do(ctx, http.MethodDelete, "/v1/apps/"+h.App+"/machines/"+id+"?force=true", nil, nil)
}
