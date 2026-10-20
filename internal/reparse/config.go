// Package reparse plans and runs a re-parse of the whole replay vault on
// temporary Fly machines, and delivers every parse to the web app. It reads
// replays from the vault only: it must not (and, by a test, does not) depend
// on the Valve client in internal/valve.
package reparse

import (
	"strconv"
	"strings"
	"time"
)

// Config is the orchestration setting; every number has a default and an env
// name (REPARSE_*), set in fly.toml [env].
type Config struct {
	Version      string // parser version of this server
	App          string // FLY_APP_NAME
	Image        string // FLY_IMAGE_REF
	Region       string
	MaxMachines  int
	CPUs         int
	MemoryMB     int
	Parallel     int // parses at once on one machine
	MaxRuntime   time.Duration
	WebURL       string
	ServiceToken string // VAULT_SERVICE_TOKEN (never logged)
	// PerSlot is how many replays one parallel slot is expected to finish.
	PerSlot int
	// Passthrough env copied to API-created machines (they do not get fly.toml [env]).
	Passthrough map[string]string
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}

// ConfigFromEnv builds the config from an env lookup.
func ConfigFromEnv(get func(string) string, version string) Config {
	c := Config{
		Version:      version,
		App:          get("FLY_APP_NAME"),
		Image:        get("FLY_IMAGE_REF"),
		Region:       get("FLY_REGION"),
		MaxMachines:  atoi(get("REPARSE_MACHINES"), 8),
		CPUs:         atoi(get("REPARSE_CPUS"), 2),
		MemoryMB:     atoi(get("REPARSE_MEMORY_MB"), 4096),
		MaxRuntime:   150 * time.Minute,
		WebURL:       strings.TrimRight(get("WEB_URL"), "/"),
		ServiceToken: get("VAULT_SERVICE_TOKEN"),
		PerSlot:      25,
	}
	if c.App == "" {
		c.App = "resoai-parse"
	}
	if c.Region == "" {
		c.Region = "arn"
	}
	c.Parallel = atoi(get("REPARSE_PARALLEL"), c.MemoryMB/2048)
	if c.Parallel < 1 {
		c.Parallel = 1
	}
	if d, err := time.ParseDuration(strings.TrimSpace(get("REPARSE_MAX_RUNTIME"))); err == nil && d > 0 {
		c.MaxRuntime = d
	}
	c.Passthrough = map[string]string{}
	for _, k := range []string{"WEB_URL", "REPARSE_MACHINES", "REPARSE_CPUS", "REPARSE_MEMORY_MB",
		"REPARSE_PARALLEL", "REPARSE_MAX_RUNTIME", "RETENTION_APPLY", "GH_REPO", "GH_WORKFLOW", "PARSER_BIN"} {
		if v := get(k); v != "" {
			c.Passthrough[k] = v
		}
	}
	return c
}

// MachinesFor is N = clamp(ceil(todo/(PerSlot*parallel)), 1, max).
func (c Config) MachinesFor(todo, override int) int {
	max := c.MaxMachines
	if override > 0 {
		max = override
	}
	per := c.PerSlot * c.Parallel
	if per < 1 {
		per = 1
	}
	n := (todo + per - 1) / per
	if n < 1 {
		n = 1
	}
	if n > max {
		n = max
	}
	return n
}
