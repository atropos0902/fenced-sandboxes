package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Blaxel sandboxes are microVMs that fall into standby when nothing is
// connected and wake on the next request, memory and processes included.
// That is why a lifetime is set in two ways below. Standby is not an end.
type Blaxel struct {
	Key, Workspace, Owner string
	API                   string // https://api.blaxel.ai/v0
	Gate                  string // host the egress proxy is limited to
	Version               string // API version header; paged lists need a recent one
	HC                    *http.Client
}

func NewBlaxel(key, workspace, owner, gate string) *Blaxel {
	return &Blaxel{Key: key, Workspace: workspace, Owner: owner, Gate: gate, API: "https://api.blaxel.ai/v0", HC: http.DefaultClient}
}

func (b *Blaxel) Name() string { return "blaxel" }

func (b *Blaxel) headers() map[string]string {
	h := map[string]string{"X-Blaxel-Authorization": "Bearer " + b.Key}
	if b.Version != "" {
		h["Blaxel-Version"] = b.Version
	}
	if b.Workspace != "" {
		h["X-Blaxel-Workspace"] = b.Workspace
	}
	return h
}

type blaxelSandbox struct {
	Metadata struct {
		Name      string            `json:"name"`
		URL       string            `json:"url"`
		Labels    map[string]string `json:"labels"`
		CreatedAt time.Time         `json:"createdAt"`
	} `json:"metadata"`
}

func (b *Blaxel) Create(ctx context.Context, s Spec) (string, error) {
	var out blaxelSandbox
	_, err := jsonCall(ctx, b.HC, "POST", b.API+"/sandboxes?createIfNotExist=true", b.headers(), map[string]any{
		"metadata": map[string]any{"name": s.Name, "labels": map[string]string{"owner": b.Owner, "run": s.RunID, "epoch": fmt.Sprint(s.Epoch)}},
		"spec": map[string]any{
			"runtime": map[string]any{"image": s.Image, "memory": s.MemoryMB, "ttl": fmt.Sprintf("%dm", max(5, int(s.TTL.Minutes())))},
			"lifecycle": map[string]any{"expirationPolicies": []map[string]string{
				{"type": "ttl-idle", "action": "delete", "value": "10m"},
			}},
			"network": map[string]any{
				"proxy":    map[string]any{"allowedDomains": []string{b.Gate}},
				"firewall": map[string]any{"rulesets": []string{"proxy"}},
			},
		},
	}, &out)
	return s.Name, err
}

func (b *Blaxel) url(ctx context.Context, ref string) (string, error) {
	var sb blaxelSandbox
	if _, err := jsonCall(ctx, b.HC, "GET", b.API+"/sandboxes/"+ref, b.headers(), nil, &sb); err != nil {
		return "", err
	}
	if sb.Metadata.URL == "" {
		return "", errors.New("sandbox has no url yet")
	}
	return strings.TrimSuffix(sb.Metadata.URL, "/"), nil
}

// Exec starts the process and then polls. A single blocking call is limited to
// 60 seconds, which is shorter than most dependency installs.
func (b *Blaxel) Exec(ctx context.Context, ref string, c Cmd) (Result, error) {
	base, err := b.url(ctx, ref)
	if err != nil {
		return Result{}, err
	}
	dir := c.Dir
	if dir == "" {
		dir = "/work"
	}
	h := map[string]string{"Authorization": "Bearer " + b.Key}
	name := fmt.Sprintf("p%d", time.Now().UnixNano())
	in := map[string]any{"name": name, "command": "sh -c " + quote(c.Shell), "workingDir": dir, "env": c.Env,
		"timeout": int(c.Timeout.Seconds()), "keepAlive": true}
	type proc struct {
		Status, Stdout, Stderr string
		ExitCode               int `json:"exitCode"`
	}
	var p proc
	if _, err := jsonCall(ctx, b.HC, "POST", base+"/process", h, in, &p); err != nil {
		return Result{}, err
	}
	start := time.Now()
	for p.Status == "running" {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(time.Second):
		}
		if _, err := jsonCall(ctx, b.HC, "GET", base+"/process/"+name, h, nil, &p); err != nil {
			return Result{}, err
		}
	}
	out, errb := &Tail{}, &Tail{}
	_, _ = out.Write([]byte(p.Stdout))
	_, _ = errb.Write([]byte(p.Stderr))
	r := Result{Exit: p.ExitCode, Out: out.String(), Err: errb.String()}
	if p.Status != "completed" && r.Exit == 0 {
		r.Exit = 1
	}
	r.TimedOut = r.Exit != 0 && c.Timeout > 0 && time.Since(start) >= c.Timeout
	return r, nil
}

func (b *Blaxel) Endpoint(ctx context.Context, ref string, port int) (Endpoint, error) {
	var p struct {
		Spec struct{ URL string } `json:"spec"`
	}
	_, err := jsonCall(ctx, b.HC, "POST", b.API+"/sandboxes/"+ref+"/previews?force=true", b.headers(), map[string]any{
		"metadata": map[string]string{"name": "app"}, "spec": map[string]any{"port": port, "public": false, "ttl": "1h"},
	}, &p)
	if err != nil {
		return Endpoint{}, err
	}
	var t struct {
		Spec struct{ Token string } `json:"spec"`
	}
	_, err = jsonCall(ctx, b.HC, "POST", b.API+"/sandboxes/"+ref+"/previews/app/tokens", b.headers(), map[string]any{
		"spec": map[string]string{"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	}, &t)
	return Endpoint{URL: p.Spec.URL, Headers: map[string]string{"X-Blaxel-Preview-Token": t.Spec.Token}}, err
}

func (b *Blaxel) List(ctx context.Context) ([]Info, error) {
	var out []Info
	cursor := ""
	for {
		// q is a substring search, so labels are checked again below.
		q := url.Values{"q": {b.Owner}, "limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var raw json.RawMessage
		if _, err := jsonCall(ctx, b.HC, "GET", b.API+"/sandboxes?"+q.Encode(), b.headers(), nil, &raw); err != nil {
			return nil, err
		}
		var page struct {
			Data []blaxelSandbox
			Meta struct {
				HasMore    bool   `json:"hasMore"`
				NextCursor string `json:"nextCursor"`
			}
		}
		var err error
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			err = json.Unmarshal(raw, &page.Data)
		} else {
			err = json.Unmarshal(raw, &page)
		}
		if err != nil {
			return nil, err
		}
		for _, s := range page.Data {
			if s.Metadata.Labels["owner"] == b.Owner {
				out = append(out, Info{Name: s.Metadata.Name, Ref: s.Metadata.Name, Running: true, Age: time.Since(s.Metadata.CreatedAt)})
			}
		}
		if !page.Meta.HasMore {
			return out, nil
		}
		cursor = page.Meta.NextCursor
	}
}

func (b *Blaxel) Destroy(ctx context.Context, name, _ string) error {
	_, err := jsonCall(ctx, b.HC, "DELETE", b.API+"/sandboxes/"+name, b.headers(), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
