package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Docker runs sandboxes on one machine through the Engine API. It is the
// provider used for development and for the end-to-end tests.
//
// Each sandbox gets its own internal network in isolated gateway mode. The
// only other member is the gate, so a sandbox has no route to the host, to
// another sandbox or to the internet except through the gate's proxy.
type Docker struct {
	Owner   string // label value; two installations can share a daemon
	Gate    string // container that joins each sandbox network as "gate"
	Runtime string // "" for runc, "runsc" for gVisor where it is installed
	hc      *http.Client
}

func NewDocker(sock, owner, gate, runtime string) *Docker {
	return &Docker{Owner: owner, Gate: gate, Runtime: runtime, hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}}
}

func (d *Docker) Name() string { return "docker" }

type apiError struct {
	Code int
	Msg  string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Msg) }

func (d *Docker) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.47"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%w: %s", ErrNotFound, strings.TrimSpace(string(b)))
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &apiError{resp.StatusCode, strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

type inspect struct {
	ID    string `json:"Id"`
	State struct{ Running bool }
}

func (d *Docker) Create(ctx context.Context, s Spec) (string, error) {
	labels := map[string]string{"fenced.owner": d.Owner, "fenced.run": s.RunID, "fenced.epoch": fmt.Sprint(s.Epoch)}
	err := d.call(ctx, "POST", "/networks/create", map[string]any{
		"Name": s.Name, "Internal": true, "Labels": labels,
		"Options": map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": "isolated"},
	}, nil)
	if err != nil && !conflict(err) {
		return "", fmt.Errorf("network: %w", err)
	}
	err = d.call(ctx, "POST", "/networks/"+s.Name+"/connect", map[string]any{
		"Container": d.Gate, "EndpointConfig": map[string]any{"Aliases": []string{"gate"}},
	}, nil)
	if err != nil && !conflict(err) && !strings.Contains(err.Error(), "already exists") {
		return "", fmt.Errorf("join gate: %w", err)
	}
	mem := int64(s.MemoryMB) << 20
	host := map[string]any{
		"NetworkMode": s.Name, "Init": true, "CapDrop": []string{"ALL"},
		"SecurityOpt": []string{"no-new-privileges"},
		"Memory":      mem, "MemorySwap": mem, "NanoCpus": int64(s.CPUs * 1e9), "PidsLimit": s.PIDs,
		"Tmpfs":     map[string]string{"/tmp": "size=256m,mode=1777"},
		"LogConfig": map[string]any{"Type": "none"},
	}
	if d.Runtime != "" {
		host["Runtime"] = d.Runtime
	}
	err = d.call(ctx, "POST", "/containers/create?name="+s.Name, map[string]any{
		"Image": s.Image, "User": "10001:10001", "Labels": labels, "HostConfig": host,
		"WorkingDir": "/work", "Entrypoint": []string{"sleep"}, "Cmd": []string{fmt.Sprint(int(s.TTL.Seconds()))},
	}, nil)
	if err != nil && !conflict(err) {
		return "", fmt.Errorf("create: %w", err)
	}
	if err := d.call(ctx, "POST", "/containers/"+s.Name+"/start", nil, nil); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}
	var in inspect
	if err := d.call(ctx, "GET", "/containers/"+s.Name+"/json", nil, &in); err != nil {
		return "", err
	}
	return in.ID, nil
}

func conflict(err error) bool {
	e, ok := err.(*apiError)
	return ok && e.Code == http.StatusConflict
}

func (d *Docker) Exec(ctx context.Context, ref string, c Cmd) (Result, error) {
	before := d.oomKills(ctx, ref)
	var r Result
	out, errb := &Tail{}, &Tail{}
	start := time.Now()
	exit, err := d.exec(ctx, ref, c, out, errb)
	if err != nil {
		return r, err
	}
	r = Result{Exit: exit, Out: out.String(), Err: errb.String()}
	r.TimedOut = c.Timeout > 0 && time.Since(start) >= c.Timeout && exit != 0
	r.OOM = exit != 0 && d.oomKills(ctx, ref) > before
	return r, nil
}

func (d *Docker) exec(ctx context.Context, ref string, c Cmd, out, errb io.Writer) (int, error) {
	argv := []string{"sh", "-c", c.Shell}
	if c.Timeout > 0 {
		argv = append([]string{"timeout", "-k", "5", fmt.Sprint(int(c.Timeout.Seconds()))}, argv...)
	}
	env := []string{"HOME=/home/sandbox"}
	for k, v := range c.Env {
		env = append(env, k+"="+v)
	}
	dir := c.Dir
	if dir == "" {
		dir = "/work"
	}
	var created struct{ Id string }
	if err := d.call(ctx, "POST", "/containers/"+ref+"/exec", map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Cmd": argv, "Env": env, "WorkingDir": dir,
	}, &created); err != nil {
		return 0, err
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://docker/v1.47/exec/"+created.Id+"/start", strings.NewReader(`{"Detach":false,"Tty":false}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, &apiError{resp.StatusCode, string(b)}
	}
	var head [8]byte
	for {
		if _, err := io.ReadFull(resp.Body, head[:]); err == io.EOF {
			break
		} else if err != nil {
			return 0, err
		}
		w := out
		if head[0] == 2 {
			w = errb
		}
		if _, err := io.CopyN(w, resp.Body, int64(binary.BigEndian.Uint32(head[4:]))); err != nil {
			return 0, err
		}
	}
	var done struct {
		Running  bool
		ExitCode int
	}
	if err := d.call(ctx, "GET", "/exec/"+created.Id+"/json", nil, &done); err != nil {
		return 0, err
	}
	if done.Running {
		// The stream closed under us, usually because the container died.
		return 0, fmt.Errorf("exec stream ended before the process did")
	}
	return done.ExitCode, nil
}

// oomKills reads the sandbox's own cgroup. Exit code 137 alone cannot tell the
// kernel's OOM killer from a plain kill -9.
func (d *Docker) oomKills(ctx context.Context, ref string) int {
	var b bytes.Buffer
	if _, err := d.exec(ctx, ref, Cmd{Shell: "grep '^oom_kill ' /sys/fs/cgroup/memory.events", Dir: "/"}, &b, io.Discard); err != nil {
		return 0
	}
	n := 0
	_, _ = fmt.Sscanf(b.String(), "oom_kill %d", &n)
	return n
}

func (d *Docker) Endpoint(ctx context.Context, ref string, port int) (Endpoint, error) {
	var in struct{ Name string }
	if err := d.call(ctx, "GET", "/containers/"+ref+"/json", nil, &in); err != nil {
		return Endpoint{}, err
	}
	return Endpoint{URL: fmt.Sprintf("http://%s:%d", strings.TrimPrefix(in.Name, "/"), port)}, nil
}

func (d *Docker) List(ctx context.Context) ([]Info, error) {
	f, _ := json.Marshal(map[string][]string{"label": {"fenced.owner=" + d.Owner}})
	q := "filters=" + url.QueryEscape(string(f))
	var cs []struct {
		Id      string
		Names   []string
		State   string
		Created int64
	}
	if err := d.call(ctx, "GET", "/containers/json?all=true&"+q, nil, &cs); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Info
	for _, c := range cs {
		n := strings.TrimPrefix(c.Names[0], "/")
		seen[n] = true
		out = append(out, Info{Name: n, Ref: c.Id, Running: c.State == "running", Age: time.Since(time.Unix(c.Created, 0))})
	}
	// A crash between the two calls in Destroy leaves a network behind, and
	// Docker runs out of address pools long before it runs out of memory.
	var ns []struct {
		Name    string
		Created time.Time
	}
	if err := d.call(ctx, "GET", "/networks?"+q, nil, &ns); err != nil {
		return nil, err
	}
	for _, n := range ns {
		if !seen[n.Name] {
			out = append(out, Info{Name: n.Name, Age: time.Since(n.Created)})
		}
	}
	return out, nil
}

func (d *Docker) Destroy(ctx context.Context, name, _ string) error {
	if err := d.call(ctx, "DELETE", "/containers/"+name+"?force=true&v=true", nil, nil); err != nil && !isNotFound(err) {
		return err
	}
	if err := d.call(ctx, "POST", "/networks/"+name+"/disconnect", map[string]any{"Container": d.Gate, "Force": true}, nil); err != nil && !isNotFound(err) {
		if e, ok := err.(*apiError); !ok || e.Code >= 500 && !strings.Contains(e.Msg, "not connected") {
			return err
		}
	}
	if err := d.call(ctx, "DELETE", "/networks/"+name, nil, nil); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func isNotFound(err error) bool { return strings.Contains(err.Error(), ErrNotFound.Error()) }

func (d *Docker) Snapshot(ctx context.Context, ref, key string) (string, error) {
	tag := key[:24]
	var out struct{ Id string }
	err := d.call(ctx, "POST", "/commit?container="+ref+"&repo=fenced-env&tag="+tag+"&pause=true", map[string]any{
		"Labels": map[string]string{"fenced.snapshot": d.Owner},
	}, &out)
	return "fenced-env:" + tag, err
}

func (d *Docker) DropSnapshot(ctx context.Context, image string) error {
	err := d.call(ctx, "DELETE", "/images/"+image, nil, nil)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}
