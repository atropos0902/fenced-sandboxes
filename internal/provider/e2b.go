package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// E2B talks to the public REST API for lifecycle and to envd inside the
// sandbox for commands. Sandboxes are Firecracker microVMs.
type E2B struct {
	Key, Owner string
	API        string // https://api.e2b.app
	EnvdURL    func(id, domain string) string
	GateCIDRs  []string // the only addresses a sandbox may reach
	HC         *http.Client

	mu   sync.Mutex
	conn map[string]e2bConn
}

type e2bConn struct{ domain, envd, traffic string }

func NewE2B(key, owner string, gateCIDRs []string) *E2B {
	return &E2B{Key: key, Owner: owner, API: "https://api.e2b.app", GateCIDRs: gateCIDRs, HC: http.DefaultClient,
		EnvdURL: func(id, domain string) string { return "https://49983-" + id + "." + domain },
		conn:    map[string]e2bConn{}}
}

func (e *E2B) Name() string { return "e2b" }

func (e *E2B) call(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	return jsonCall(ctx, e.HC, method, e.API+path, map[string]string{"X-API-Key": e.Key}, in, out)
}

type e2bSandbox struct {
	SandboxID          string            `json:"sandboxID"`
	State              string            `json:"state"`
	Domain             string            `json:"domain"`
	EnvdAccessToken    string            `json:"envdAccessToken"`
	TrafficAccessToken string            `json:"trafficAccessToken"`
	Metadata           map[string]string `json:"metadata"`
	StartedAt          time.Time         `json:"startedAt"`
}

// Create looks for the name first because E2B has no unique names: a create
// that timed out and is simply repeated leaves two sandboxes running.
func (e *E2B) Create(ctx context.Context, s Spec) (string, error) {
	found, err := e.list(ctx, "name="+url.QueryEscape(s.Name))
	if err != nil {
		return "", err
	}
	for _, f := range found {
		if err := e.kill(ctx, f.Ref); err != nil {
			return "", fmt.Errorf("remove earlier create: %w", err)
		}
	}
	var sb e2bSandbox
	_, err = e.call(ctx, "POST", "/sandboxes", map[string]any{
		"templateID": s.Image,
		"timeout":    int(s.TTL.Seconds()),
		"autoPause":  false, // end of life means gone. A paused sandbox can be resumed.
		"secure":     true,
		"metadata":   map[string]string{"owner": e.Owner, "name": s.Name, "run": s.RunID, "epoch": fmt.Sprint(s.Epoch)},
		"network": map[string]any{
			"allowPublicTraffic": false, "allowOut": e.GateCIDRs, "denyOut": []string{"0.0.0.0/0"},
		},
	}, &sb)
	if err != nil {
		return "", err
	}
	if sb.Domain == "" {
		sb.Domain = "e2b.app"
	}
	e.mu.Lock()
	e.conn[sb.SandboxID] = e2bConn{sb.Domain, sb.EnvdAccessToken, sb.TrafficAccessToken}
	e.mu.Unlock()
	return sb.SandboxID, nil
}

func (e *E2B) get(ref string) (e2bConn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.conn[ref]
	if !ok {
		// Tokens are only returned at create and are never stored. A worker that
		// lost them starts a new attempt instead of adopting this sandbox.
		return c, ErrNotFound
	}
	return c, nil
}

// Exec speaks the Connect protocol's server stream: five bytes of header, one
// flag byte and a big-endian length, then a JSON message.
func (e *E2B) Exec(ctx context.Context, ref string, c Cmd) (Result, error) {
	conn, err := e.get(ref)
	if err != nil {
		return Result{}, err
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout+10*time.Second)
		defer cancel()
	}
	dir := c.Dir
	if dir == "" {
		dir = "/work"
	}
	cmd, args := "sh", []string{"-c", c.Shell}
	if c.Timeout > 0 {
		cmd, args = "timeout", append([]string{"-k", "5", fmt.Sprint(int(c.Timeout.Seconds())), "sh"}, args...)
	}
	msg, _ := json.Marshal(map[string]any{"process": map[string]any{"cmd": cmd, "args": args, "envs": c.Env, "cwd": dir}})
	body := make([]byte, 5, 5+len(msg))
	binary.BigEndian.PutUint32(body[1:], uint32(len(msg)))
	req, err := http.NewRequestWithContext(ctx, "POST", e.EnvdURL(ref, conn.domain)+"/process.Process/Start", bytes.NewReader(append(body, msg...)))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("X-Access-Token", conn.envd)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:")))
	start := time.Now()
	resp, err := e.HC.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return Result{}, &apiError{resp.StatusCode, string(b)}
	}
	out, errb := &Tail{}, &Tail{}
	for {
		var head [5]byte
		if _, err := io.ReadFull(resp.Body, head[:]); err != nil {
			return Result{}, fmt.Errorf("stream ended without an exit code: %w", err)
		}
		n := binary.BigEndian.Uint32(head[1:])
		if n > 8<<20 {
			return Result{}, fmt.Errorf("message of %d bytes", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			return Result{}, err
		}
		if head[0]&2 != 0 {
			return Result{}, fmt.Errorf("stream closed early: %s", buf)
		}
		var m struct {
			Event struct {
				Data *struct{ Stdout, Stderr []byte }
				End  *struct {
					ExitCode int    `json:"exitCode"`
					Exited   bool   `json:"exited"`
					Status   string `json:"status"`
				}
			}
		}
		if err := json.Unmarshal(buf, &m); err != nil {
			return Result{}, err
		}
		if d := m.Event.Data; d != nil {
			_, _ = out.Write(d.Stdout)
			_, _ = errb.Write(d.Stderr)
		}
		if end := m.Event.End; end != nil {
			exit := end.ExitCode
			if !end.Exited && exit == 0 {
				exit = 137 // killed by a signal, e.g. status "signal: killed"
			}
			return Result{Exit: exit, Out: out.String(), Err: errb.String(),
				TimedOut: c.Timeout > 0 && time.Since(start) >= c.Timeout && exit != 0}, nil
		}
	}
}

func (e *E2B) Endpoint(_ context.Context, ref string, port int) (Endpoint, error) {
	c, err := e.get(ref)
	if err != nil {
		return Endpoint{}, err
	}
	ep := Endpoint{URL: fmt.Sprintf("https://%d-%s.%s", port, ref, c.domain)}
	if c.traffic != "" {
		ep.Headers = map[string]string{"E2b-Traffic-Access-Token": c.traffic}
	}
	return ep, nil
}

func (e *E2B) List(ctx context.Context) ([]Info, error) {
	return e.list(ctx, "owner="+url.QueryEscape(e.Owner))
}

// list asks for paused sandboxes too. GET /sandboxes returns running ones only,
// so a leaked paused sandbox would never show up there.
func (e *E2B) list(ctx context.Context, meta string) ([]Info, error) {
	var out []Info
	next := ""
	for {
		q := url.Values{"metadata": {meta}, "state": {"running,paused"}, "limit": {"100"}}
		if next != "" {
			q.Set("nextToken", next)
		}
		var page []e2bSandbox
		h, err := e.call(ctx, "GET", "/v2/sandboxes?"+q.Encode(), nil, &page)
		if err != nil {
			return nil, err
		}
		for _, s := range page {
			if s.Metadata["owner"] != e.Owner {
				continue
			}
			out = append(out, Info{Name: s.Metadata["name"], Ref: s.SandboxID, Running: s.State == "running", Age: time.Since(s.StartedAt)})
		}
		if next = h.Get("X-Next-Token"); next == "" {
			return out, nil
		}
	}
}

func (e *E2B) kill(ctx context.Context, id string) error {
	_, err := e.call(ctx, "DELETE", "/sandboxes/"+id, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (e *E2B) Destroy(ctx context.Context, name, ref string) error {
	if ref != "" {
		e.mu.Lock()
		delete(e.conn, ref)
		e.mu.Unlock()
		return e.kill(ctx, ref)
	}
	found, err := e.list(ctx, "name="+url.QueryEscape(name))
	for _, f := range found {
		if err := e.kill(ctx, f.Ref); err != nil {
			return err
		}
	}
	return err
}

func (e *E2B) Snapshot(ctx context.Context, ref, _ string) (string, error) {
	var out struct {
		SnapshotID string `json:"snapshotID"`
	}
	_, err := e.call(ctx, "POST", "/sandboxes/"+ref+"/snapshots", map[string]any{}, &out)
	if err == nil && out.SnapshotID == "" {
		err = errors.New("no snapshotID in response")
	}
	return out.SnapshotID, err
}

func (e *E2B) DropSnapshot(ctx context.Context, image string) error {
	_, err := e.call(ctx, "DELETE", "/templates/"+url.PathEscape(image), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func jsonCall(ctx context.Context, hc *http.Client, method, u string, headers map[string]string, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &apiError{resp.StatusCode, strings.TrimSpace(string(b))}
	}
	if out != nil {
		return resp.Header, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.Header, nil
}
