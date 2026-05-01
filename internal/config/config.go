// Package config reads the operator's repository list. Secrets are named here
// and read from the environment, so the file itself can be committed.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Cred struct {
	Name      string `json:"name"`       // path segment under /cred/
	Base      string `json:"base"`       // fixed upstream, never chosen by the sandbox
	Header    string `json:"header"`     // header that carries the real key
	Prefix    string `json:"prefix"`     // e.g. "Bearer "
	SecretEnv string `json:"secret_env"` // environment variable that holds the key
}

type Limits struct {
	MemoryMB int     `json:"memory_mb"`
	CPUs     float64 `json:"cpus"`
	PIDs     int64   `json:"pids"`
}

type Repo struct {
	Name        string            `json:"name"` // owner/repo
	Base        string            `json:"base"`
	Image       string            `json:"image"`
	Setup       string            `json:"setup"`
	Check       string            `json:"check"`
	Preview     string            `json:"preview"`
	PreviewPort int               `json:"preview_port"`
	SetupInputs []string          `json:"setup_inputs"` // files that decide what setup does
	AllowHosts  []string          `json:"allow_hosts"`
	Env         map[string]string `json:"env"` // plain values only
	Creds       []Cred            `json:"creds"`
	Limits      Limits            `json:"limits"`
	SetupSecs   int               `json:"setup_seconds"`
	AgentSecs   int               `json:"agent_seconds"`
	CheckSecs   int               `json:"check_seconds"`
	PreviewSecs int               `json:"preview_seconds"`
}

type Config struct {
	Repos []Repo `json:"repos"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range c.Repos {
		r := &c.Repos[i]
		if strings.Count(r.Name, "/") != 1 || strings.Contains(r.Name, "..") {
			return nil, fmt.Errorf("repo %q: want owner/name", r.Name)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("repo %q listed twice", r.Name)
		}
		seen[r.Name] = true
		if r.Image == "" || r.Base == "" {
			return nil, fmt.Errorf("repo %q: image and base are required", r.Name)
		}
		for _, h := range r.AllowHosts {
			if h == "*" || strings.ContainsAny(h, "/:") {
				return nil, fmt.Errorf("repo %q: allow_hosts entry %q must be a host name", r.Name, h)
			}
		}
		def := func(p *int, v int) {
			if *p == 0 {
				*p = v
			}
		}
		def(&r.SetupSecs, 300)
		def(&r.AgentSecs, 1800)
		def(&r.CheckSecs, 600)
		def(&r.PreviewSecs, 60)
		def(&r.Limits.MemoryMB, 2048)
		if r.Limits.CPUs == 0 {
			r.Limits.CPUs = 1
		}
		if r.Limits.PIDs == 0 {
			r.Limits.PIDs = 512
		}
	}
	return &c, nil
}

func (c *Config) Repo(name string) *Repo {
	for i := range c.Repos {
		if c.Repos[i].Name == name {
			return &c.Repos[i]
		}
	}
	return nil
}

func (r *Repo) Cred(name string) *Cred {
	for i := range r.Creds {
		if r.Creds[i].Name == name {
			return &r.Creds[i]
		}
	}
	return nil
}
