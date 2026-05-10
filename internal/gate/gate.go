// Package gate is the only thing a sandbox can reach. It holds the real
// credentials, sandboxes hold a token that is good for one attempt, and each
// request is checked against the database. The gate has no access to the
// sandbox provider and no Docker socket.
package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"fenced/internal/config"
	"fenced/internal/obs"
	"fenced/internal/store"
)

// PushTimeout must stay under the worker's lease. The run's row is locked for
// this long at most.
const PushTimeout = 20 * time.Second

type Upstream interface {
	// GitURL is the https base of the repository and the Authorization value.
	GitURL(ctx context.Context, repo string) (base, authorization string, err error)
}

type Gate struct {
	Store  *store.Store
	Config *config.Config
	Up     Upstream
	HC     *http.Client
	Log    *slog.Logger

	MaxPush int64 // bytes; 256 MiB if zero
}

func (g *Gate) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /git/{owner}/{repo}/info/refs", g.refs)
	m.HandleFunc("POST /git/{owner}/{repo}/git-upload-pack", g.fetch)
	m.HandleFunc("POST /git/{owner}/{repo}/git-receive-pack", g.push)
	m.HandleFunc("/cred/{name}/{rest...}", g.cred)
	return m
}

// grant accepts the token wherever the tool in the sandbox puts its key.
func (g *Gate) grant(r *http.Request) (*store.Grant, error) {
	tok := r.Header.Get("X-Api-Key")
	if _, p, ok := r.BasicAuth(); ok {
		tok = p
	} else if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		tok = a[7:]
	}
	if tok == "" {
		return nil, store.ErrFenced
	}
	return g.Store.Grant(r.Context(), tok)
}

func (g *Gate) git(w http.ResponseWriter, r *http.Request, capability string) (*store.Grant, string, string, bool) {
	gr, err := g.grant(r)
	if err != nil {
		obs.GateRejected.WithLabelValues("stale_token").Inc()
		w.Header().Set("WWW-Authenticate", `Basic realm="sandbox"`)
		http.Error(w, "token is not valid for a current attempt", http.StatusUnauthorized)
		return nil, "", "", false
	}
	repo := r.PathValue("owner") + "/" + strings.TrimSuffix(r.PathValue("repo"), ".git")
	if repo != gr.Repo || !gr.Can(capability) {
		obs.GateRejected.WithLabelValues("not_granted").Inc()
		http.Error(w, "this sandbox may not do that", http.StatusForbidden)
		return nil, "", "", false
	}
	base, auth, err := g.Up.GitURL(r.Context(), repo)
	if err != nil {
		g.Log.Error("upstream credentials", "repo", repo, "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return nil, "", "", false
	}
	return gr, base, auth, true
}

func (g *Gate) refs(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("service")
	capability := map[string]string{"git-upload-pack": "fetch", "git-receive-pack": "push"}[svc]
	if capability == "" {
		http.Error(w, "unknown service", http.StatusBadRequest)
		return
	}
	_, base, auth, ok := g.git(w, r, capability)
	if !ok {
		return
	}
	g.forward(w, r, base+"/info/refs?service="+svc, auth, nil)
}

func (g *Gate) fetch(w http.ResponseWriter, r *http.Request) {
	_, base, auth, ok := g.git(w, r, "fetch")
	if !ok {
		return
	}
	g.forward(w, r, base+"/git-upload-pack", auth, r.Body)
}

func (g *Gate) forward(w http.ResponseWriter, r *http.Request, u, auth string, body io.Reader) {
	resp, err := g.send(r.Context(), r, u, auth, body, r.ContentLength)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	relay(w, resp)
}

func (g *Gate) send(ctx context.Context, r *http.Request, u, auth string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, r.Method, u, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	for _, h := range []string{"Content-Type", "Content-Encoding", "Accept", "Git-Protocol", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return g.HC.Do(req)
}

func relay(w http.ResponseWriter, resp *http.Response) {
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// push lets one ref update through: the run's own branch, from the commit we
// recorded, while this attempt still owns the run.
func (g *Gate) push(w http.ResponseWriter, r *http.Request) {
	gr, base, auth, ok := g.git(w, r, "push")
	if !ok {
		return
	}
	if r.Header.Get("Content-Encoding") != "" {
		http.Error(w, "encoded push", http.StatusUnsupportedMediaType)
		return
	}
	limit := g.MaxPush
	if limit == 0 {
		limit = 256 << 20
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	cmds, raw, err := ReadCommands(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if len(cmds) == 0 {
		return // git's empty probe before a large push
	}
	refuse := func(ref, why string) {
		obs.GateRejected.WithLabelValues("push_refused").Inc()
		_ = g.Store.Event(r.Context(), gr.RunID, gr.Epoch, "push_refused", map[string]any{"ref": ref, "why": why, "sandbox": gr.Sandbox})
		http.Error(w, why, http.StatusForbidden)
	}
	c := cmds[0]
	if len(cmds) != 1 || c.Ref != "refs/heads/agent/"+gr.RunID {
		refuse(c.Ref, "only refs/heads/agent/"+gr.RunID+" may be updated")
		return
	}
	if c.New == zeroSHA {
		refuse(c.Ref, "the branch may not be deleted")
		return
	}
	// The pack is spooled first. A sandbox that uploads one byte a second must
	// not get to hold the row lock while it does.
	spool, err := os.CreateTemp("", "pack-")
	if err != nil {
		http.Error(w, "no space", http.StatusInsufficientStorage)
		return
	}
	defer func() { spool.Close(); os.Remove(spool.Name()) }()
	size, err := io.Copy(spool, body)
	if err != nil {
		http.Error(w, "upload ended early", http.StatusBadRequest)
		return
	}

	// The client going away must not cancel an update that is already upstream.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), PushTimeout)
	defer cancel()
	var resp *http.Response
	err = g.Store.Push(ctx, gr, func(run *store.Run) (string, error) {
		old := run.HeadSHA
		if old == "" {
			old = zeroSHA
		}
		if c.Old != old {
			return "", fmt.Errorf("%w: branch is recorded at %s", store.ErrConflict, old)
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		sctx, scancel := context.WithTimeout(ctx, PushTimeout*3/5)
		defer scancel()
		var sendErr error
		resp, sendErr = g.send(sctx, r, base+"/git-receive-pack", auth, io.MultiReader(strings.NewReader(string(raw)), spool), int64(len(raw))+size)
		var report []byte
		if sendErr == nil {
			report, sendErr = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			resp.Body = io.NopCloser(strings.NewReader(string(report)))
		}
		// A 200 can carry a refusal and a timeout can hide a success, so the
		// branch itself is the only answer that counts.
		at, err := g.upstreamRef(ctx, base, auth, c.Ref)
		switch {
		case err != nil:
			return "", fmt.Errorf("%w: %v", store.ErrUnknown, err)
		case at == c.New:
			return c.New, sendErr
		case at == "" && old == zeroSHA, at == old:
			if sendErr != nil {
				// Still in flight upstream, perhaps. Not safe to call it failed.
				return "", fmt.Errorf("%w: %v", store.ErrUnknown, sendErr)
			}
			return "", nil
		default:
			return "", fmt.Errorf("%w: branch moved to %s", store.ErrUnknown, at)
		}
	})
	switch {
	case errors.Is(err, store.ErrFenced):
		obs.StalePush.Inc()
		refuse(c.Ref, "this attempt has been replaced")
	case errors.Is(err, store.ErrConflict):
		refuse(c.Ref, err.Error())
	case err != nil:
		g.Log.Error("push", "run", gr.RunID, "epoch", gr.Epoch, "err", err)
		http.Error(w, "push outcome could not be confirmed", http.StatusBadGateway)
	default:
		relay(w, resp)
	}
}

func (g *Gate) upstreamRef(ctx context.Context, base, auth, ref string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return "", err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := g.HC.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("info/refs: HTTP %d", resp.StatusCode)
	}
	refs, err := ParseRefs(resp.Body)
	return refs[ref], err
}

// cred proxies to one fixed upstream and swaps the sandbox token for the key.
func (g *Gate) cred(w http.ResponseWriter, r *http.Request) {
	gr, err := g.grant(r)
	if err != nil {
		obs.GateRejected.WithLabelValues("stale_token").Inc()
		http.Error(w, "token is not valid for a current attempt", http.StatusUnauthorized)
		return
	}
	repo := g.Config.Repo(gr.Repo)
	var c *config.Cred
	if repo != nil && gr.Can("cred") {
		c = repo.Cred(r.PathValue("name"))
	}
	if c == nil {
		obs.GateRejected.WithLabelValues("not_granted").Inc()
		http.Error(w, "this sandbox may not do that", http.StatusForbidden)
		return
	}
	base, err := url.Parse(c.Base)
	if err != nil {
		http.Error(w, "bad upstream", http.StatusInternalServerError)
		return
	}
	(&httputil.ReverseProxy{
		FlushInterval: -1, // model responses stream
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme, p.Out.URL.Host, p.Out.Host = base.Scheme, base.Host, base.Host
			p.Out.URL.Path = strings.TrimSuffix(base.Path, "/") + "/" + r.PathValue("rest")
			p.Out.URL.RawPath = ""
			for _, h := range []string{"Authorization", "X-Api-Key", "Cookie"} {
				p.Out.Header.Del(h)
			}
			p.Out.Header.Set(c.Header, c.Prefix+os.Getenv(c.SecretEnv))
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			return nil
		},
	}).ServeHTTP(w, r)
}
