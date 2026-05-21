// Package worker runs attempts. A worker owns a run only while its epoch is the
// run's epoch, and it learns that it has lost the run from the database, not
// from its own clock.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"fenced/internal/config"
	"fenced/internal/diagnose"
	"fenced/internal/obs"
	"fenced/internal/provider"
	"fenced/internal/store"
)

type GitHub interface {
	BranchSHA(ctx context.Context, repo, branch string) (string, error)
	Blobs(ctx context.Context, repo, sha string) (map[string]string, error)
	EnsurePR(ctx context.Context, repo, head, base, title, body string) (int, error)
	Check(ctx context.Context, repo, sha, name, conclusion, summary string) error
}

type Worker struct {
	ID         string
	Store      *store.Store
	Config     *config.Config
	Provider   provider.Provider
	GitHub     GitHub
	Log        *slog.Logger
	GateURL    string // as seen from a sandbox
	ProxyHost  string // host:port of the egress proxy as seen from a sandbox
	Lease      time.Duration
	PreviewTTL time.Duration
	// LeaveOld is a fault for the tests: a takeover does not destroy the
	// previous sandbox, as if the provider's delete call were failing.
	LeaveOld bool
}

func (w *Worker) Loop(ctx context.Context) {
	for ctx.Err() == nil {
		r, err := w.Store.Claim(ctx, w.ID, w.Lease)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("claim", "err", err)
		}
		if r == nil {
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		w.run(ctx, r)
	}
}

// failure is a diagnosed stop. Anything else that comes back is our own error.
type failure struct{ diagnose.Cause }

func (f *failure) Error() string { return f.Code }

func (w *Worker) run(parent context.Context, r *store.Run) {
	log := w.Log.With("run", r.ID, "epoch", r.Epoch)
	if r.Epoch > 1 {
		obs.Takeovers.Inc()
		if !w.LeaveOld {
			w.destroyOlder(parent, r)
		}
	}
	if r.State == "failed" {
		return
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	go w.renew(ctx, r, cancel)

	ctx, span := obs.Tracer.Start(ctx, "attempt", trace.WithAttributes(
		attribute.String("run", r.ID), attribute.Int("epoch", r.Epoch), attribute.String("repo", r.Repo)))
	err := w.attempt(ctx, r)
	if errors.Is(context.Cause(ctx), store.ErrFenced) {
		err = store.ErrFenced
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()

	// From here on use the parent: cleaning up after a fenced attempt is still ours.
	bg, done := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer done()
	var f *failure
	switch {
	case err == nil:
		log.Info("ready for review")
		return
	case errors.Is(err, store.ErrFenced):
		log.Warn("lost the run")
	case errors.As(err, &f):
		obs.Failures.WithLabelValues(f.Code, f.Owner).Inc()
		log.Warn("attempt failed", "code", f.Code, "owner", f.Owner, "evidence", f.Evidence)
		_ = w.Store.Event(bg, r.ID, r.Epoch, "failed", map[string]any{"cause": f.Cause})
		if f.Owner == diagnose.Platform && r.Epoch < r.MaxAttempts {
			_ = w.Store.Requeue(bg, r)
		} else {
			_ = w.Store.Fail(bg, r, f.Code, f.Owner, f.Cause)
		}
	case parent.Err() != nil:
		log.Info("shutting down, handing the run back")
		_ = w.Store.Requeue(bg, r)
	default:
		obs.Failures.WithLabelValues("internal", diagnose.Platform).Inc()
		log.Error("attempt error", "err", err)
		_ = w.Store.Event(bg, r.ID, r.Epoch, "error", map[string]any{"err": err.Error()})
		if r.Epoch < r.MaxAttempts {
			_ = w.Store.Requeue(bg, r)
		} else {
			_ = w.Store.Fail(bg, r, "internal", diagnose.Platform, map[string]any{"err": err.Error()})
		}
	}
	for _, role := range []string{"build", "agent", "verify"} {
		w.drop(bg, r, role)
	}
}

func (w *Worker) renew(ctx context.Context, r *store.Run, cancel context.CancelCauseFunc) {
	t := time.NewTicker(w.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Store.Renew(ctx, r, w.Lease); errors.Is(err, store.ErrFenced) {
				cancel(store.ErrFenced)
				return
			}
		}
	}
}

func (w *Worker) destroyOlder(ctx context.Context, r *store.Run) {
	open, err := w.Store.Open(ctx)
	if err != nil {
		return
	}
	for _, s := range open {
		if s.RunID == r.ID && s.Epoch < r.Epoch {
			if err := w.Provider.Destroy(ctx, s.Name, s.Ref); err != nil {
				w.Log.Warn("destroy older sandbox", "name", s.Name, "err", err) // the reconciler will get it
				continue
			}
			_ = w.Store.Destroyed(ctx, s.Name)
		}
	}
}

func (w *Worker) drop(ctx context.Context, r *store.Run, role string) {
	name := store.SandboxName(r.ID, r.Epoch, role)
	_ = w.Store.Unwant(ctx, name)
	if err := w.Provider.Destroy(ctx, name, ""); err != nil {
		w.Log.Warn("destroy", "name", name, "err", err)
		return
	}
	_ = w.Store.Destroyed(ctx, name)
}

type box struct {
	name, ref, token string
}

func (w *Worker) create(ctx context.Context, r *store.Run, repo *config.Repo, role, image string, caps ...string) (*box, error) {
	name, token, err := w.Store.Intend(ctx, r, role, w.Provider.Name(), caps)
	if err != nil {
		return nil, err
	}
	ref, err := w.Provider.Create(ctx, provider.Spec{Name: name, RunID: r.ID, Epoch: r.Epoch, Image: image,
		MemoryMB: repo.Limits.MemoryMB, CPUs: repo.Limits.CPUs, PIDs: repo.Limits.PIDs, TTL: w.PreviewTTL + time.Hour})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &failure{diagnose.Cause{Code: "provider_create_failed", Owner: diagnose.Platform, Evidence: []string{err.Error()}, Next: "Retried on a new sandbox."}}
	}
	return &box{name, ref, token}, w.Store.Live(ctx, name, ref)
}

// env is everything a command in the sandbox is given. The proxy and git
// settings are passed per command and never written into the image.
func (w *Worker) env(repo *config.Repo, b *box) map[string]string {
	proxy := "http://run:" + b.token + "@" + w.ProxyHost
	gateHost := strings.TrimPrefix(strings.TrimPrefix(w.GateURL, "https://"), "http://")
	gateHost, _, _ = strings.Cut(gateHost, ":")
	e := map[string]string{
		"HTTP_PROXY": proxy, "HTTPS_PROXY": proxy, "http_proxy": proxy, "https_proxy": proxy,
		"NO_PROXY": gateHost + ",localhost,127.0.0.1", "no_proxy": gateHost + ",localhost,127.0.0.1",
		"GIT_TERMINAL_PROMPT": "0", "CI": "true",
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "http." + w.GateURL + "/.extraHeader",
		"GIT_CONFIG_VALUE_0": "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("run:"+b.token)),
		"FENCED_TOKEN":       b.token,
		"FENCED_GATE":        w.GateURL,
	}
	for k, v := range repo.Env {
		e[k] = v
	}
	return e
}

func (w *Worker) phase(ctx context.Context, r *store.Run, b *box, phase, shell, dir string, env map[string]string, secs int) error {
	ctx, span := obs.Tracer.Start(ctx, phase)
	defer span.End()
	if err := w.Store.SetPhase(ctx, b.name, phase); err != nil {
		return err
	}
	start := time.Now()
	res, err := w.Provider.Exec(ctx, b.ref, provider.Cmd{Shell: shell, Dir: dir, Env: env, Timeout: time.Duration(secs) * time.Second})
	outcome := "ok"
	if err != nil || res.Exit != 0 {
		outcome = "failed"
		span.SetStatus(codes.Error, phase)
	}
	obs.Phase.WithLabelValues(phase, outcome).Observe(time.Since(start).Seconds())
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return w.lost(ctx, b, err)
	}
	_ = w.Store.Event(ctx, r.ID, r.Epoch, "phase", map[string]any{"phase": phase, "sandbox": b.name, "exit": res.Exit,
		"ms": time.Since(start).Milliseconds(), "out": tail(res.Out, 4000), "err": tail(res.Err, 4000)})
	if res.Exit == 0 {
		return nil
	}
	if res.Exit > 128 && !res.OOM && !res.TimedOut {
		// Killed by a signal. A sandbox that was taken away looks the same from
		// here as a program that died, and only one of those is the agent's doing.
		var f *failure
		if err := w.lost(ctx, b, nil); errors.As(err, &f) {
			return f
		}
	}
	denied, _ := w.Store.Denied(ctx, r.ID, r.Epoch, b.name, phase)
	return &failure{diagnose.Explain(diagnose.Facts{Phase: phase, Exit: res.Exit, Out: res.Out, Err: res.Err, TimedOut: res.TimedOut, OOM: res.OOM, Denied: denied,
		Changed: strings.HasSuffix(b.name, "-verify")})}
}

// lost separates "the sandbox is gone" from "our call failed".
func (w *Worker) lost(ctx context.Context, b *box, cause error) error {
	list, err := w.Provider.List(ctx)
	if err != nil {
		return cause
	}
	for _, i := range list {
		if i.Name == b.name && i.Running {
			return cause
		}
	}
	obs.Lost.Inc()
	return &failure{diagnose.Explain(diagnose.Facts{Lost: true})}
}

const workdir = "/work/repo"

func (w *Worker) clone(ref, sha string) string {
	// A snapshot already has a checkout. reset and clean keep ignored files,
	// which is where installed dependencies live.
	return fmt.Sprintf(`set -e
if [ -d %[1]s/.git ]; then cd %[1]s; else git init -q -b sandbox %[1]s; cd %[1]s; git remote add origin "$REMOTE"; fi
git remote set-url origin "$REMOTE"
git fetch -q --depth 50 origin %[2]s
test "$(git rev-parse FETCH_HEAD)" = %[3]s || { echo "branch is not at the expected commit" >&2; exit 3; }
git checkout -q -f -B sandbox FETCH_HEAD
git clean -qfd`, workdir, ref, sha)
}

func (w *Worker) attempt(ctx context.Context, r *store.Run) error {
	repo := w.Config.Repo(r.Repo)
	if repo == nil {
		return &failure{diagnose.Cause{Code: "unknown_repo", Owner: diagnose.Setup, Next: "Add the repository to the configuration."}}
	}
	base, err := w.GitHub.BranchSHA(ctx, r.Repo, repo.Base)
	if err != nil {
		return err
	}
	if err := w.Store.SetBase(ctx, r, base); err != nil {
		return err
	}
	image, err := w.image(ctx, r, repo, base)
	if err != nil {
		return err
	}
	head, err := w.agent(ctx, r, repo, image, base)
	if err != nil {
		return err
	}
	return w.verify(ctx, r, repo, image, head)
}

func (w *Worker) remote(repo *config.Repo, env map[string]string) map[string]string {
	env["REMOTE"] = w.GateURL + "/git/" + repo.Name + ".git"
	return env
}

// key names everything setup depends on. Files are identified by the blob ids
// GitHub reports, so nothing a sandbox computed can end up in it.
func (w *Worker) key(ctx context.Context, repo *config.Repo, sha string) (string, error) {
	blobs, err := w.GitHub.Blobs(ctx, repo.Name, sha)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n%s\n", w.Provider.Name(), repo.Name, repo.Image, repo.Setup)
	for _, f := range repo.SetupInputs {
		fmt.Fprintf(h, "%s=%s\n", f, blobs[f])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// prepared reports whether image already holds what setup would do at sha.
func (w *Worker) prepared(ctx context.Context, repo *config.Repo, image, sha string) bool {
	if image == repo.Image {
		return false
	}
	key, err := w.key(ctx, repo, sha)
	if err != nil {
		return false
	}
	ref, err := w.Store.Snapshot(ctx, key)
	return err == nil && ref == image
}

// image returns a prepared environment for these setup inputs, building it once.
// Only the base branch is ever built from, so an agent's run cannot change what
// later runs start with.
func (w *Worker) image(ctx context.Context, r *store.Run, repo *config.Repo, base string) (string, error) {
	snap, ok := w.Provider.(provider.Snapshotter)
	if !ok || repo.Setup == "" {
		return repo.Image, nil
	}
	key, err := w.key(ctx, repo, base)
	if err != nil {
		w.Log.Warn("no cache key, starting cold", "repo", r.Repo, "err", err)
		return repo.Image, nil
	}
	if ref, err := w.Store.Snapshot(ctx, key); err != nil || ref != "" {
		return ref, err
	}
	b, err := w.create(ctx, r, repo, "build", repo.Image, "fetch", "egress")
	if err != nil {
		return "", err
	}
	env := w.remote(repo, w.env(repo, b))
	if err := w.phase(ctx, r, b, "clone", w.clone(repo.Base, base), "/work", env, 120); err != nil {
		return "", err
	}
	if err := w.phase(ctx, r, b, "setup", repo.Setup, workdir, env, repo.SetupSecs); err != nil {
		return "", err
	}
	// Revoked before the copy is taken. If setup wrote the token to disk, what
	// the snapshot holds is already dead.
	if err := w.Store.Revoke(ctx, b.name); err != nil {
		return "", err
	}
	ref, err := snap.Snapshot(ctx, b.ref, key)
	if err != nil {
		w.Log.Warn("snapshot", "err", err)
		ref = repo.Image // slower, still correct
	} else if err := w.Store.PutSnapshot(ctx, key, repo.Name, w.Provider.Name(), ref); err != nil {
		return "", err
	}
	w.drop(ctx, r, "build")
	return ref, nil
}

// ready brings a sandbox to the point where work can start.
func (w *Worker) ready(ctx context.Context, r *store.Run, repo *config.Repo, role, image, ref, sha string, caps ...string) (*box, map[string]string, error) {
	start, kind := time.Now(), "cold"
	if w.prepared(ctx, repo, image, sha) {
		kind = "warm"
	}
	b, err := w.create(ctx, r, repo, role, image, caps...)
	if err != nil {
		return nil, nil, err
	}
	env := w.remote(repo, w.env(repo, b))
	if err := w.phase(ctx, r, b, "clone", w.clone(ref, sha), "/work", env, 120); err != nil {
		return nil, nil, err
	}
	// When the agent changed a setup input, the clean sandbox installs again.
	if repo.Setup != "" && kind == "cold" {
		if err := w.phase(ctx, r, b, "setup", repo.Setup, workdir, env, repo.SetupSecs); err != nil {
			return nil, nil, err
		}
	}
	obs.Start.WithLabelValues(kind).Observe(time.Since(start).Seconds())
	_ = w.Store.Event(ctx, r.ID, r.Epoch, "ready", map[string]any{"role": role, "start": kind, "ms": time.Since(start).Milliseconds()})
	return b, env, nil
}

func (w *Worker) agent(ctx context.Context, r *store.Run, repo *config.Repo, image, base string) (string, error) {
	ref, sha := repo.Base, base
	if r.HeadSHA != "" { // an earlier attempt pushed before it was replaced
		ref, sha = r.Branch(), r.HeadSHA
	}
	b, env, err := w.ready(ctx, r, repo, "agent", image, ref, sha, "fetch", "push", "egress", "cred")
	if err != nil {
		return "", err
	}
	for _, c := range repo.Creds {
		env["FENCED_CRED_"+strings.ToUpper(c.Name)] = w.GateURL + "/cred/" + c.Name
	}
	if err := w.phase(ctx, r, b, "agent", r.AgentCmd, workdir, env, repo.AgentSecs); err != nil {
		return "", err
	}
	push := `set -e
git add -A
git -c user.name=sandbox -c user.email=sandbox@localhost diff --cached --quiet || git -c user.name=sandbox -c user.email=sandbox@localhost commit -q -m "$TITLE"
test "$(git rev-parse HEAD)" != "$BASE" || { echo no-changes >&2; exit 10; }
test "$(git rev-parse HEAD)" = "$RECORDED" || git push -q origin HEAD:` + r.Branch()
	env["TITLE"], env["BASE"], env["RECORDED"] = r.Title, base, r.HeadSHA
	err = w.phase(ctx, r, b, "push", push, workdir, env, 120)
	var f *failure
	if errors.As(err, &f) {
		cur, e := w.Store.GetRun(ctx, r.ID)
		if e == nil && cur.Epoch != r.Epoch {
			return "", store.ErrFenced
		}
		res := diagnose.Cause{Code: "push_failed", Owner: diagnose.Platform, Evidence: f.Evidence, Next: "Retried on a new sandbox."}
		if cur != nil && cur.HeadSHA == "" && strings.Contains(strings.Join(f.Evidence, ""), "no-changes") {
			res = diagnose.Cause{Code: "no_changes", Owner: diagnose.Agent, Next: "The agent finished without changing a file."}
		}
		return "", &failure{res}
	}
	if err != nil {
		return "", err
	}
	// The agent's sandbox is finished with. Its token stops working here, before
	// anyone is asked to look at the commit, and the memory is free for the check.
	if err := w.Store.Revoke(ctx, b.name); err != nil {
		return "", err
	}
	w.drop(ctx, r, "agent")
	cur, err := w.Store.GetRun(ctx, r.ID)
	if err != nil {
		return "", err
	}
	if cur.Epoch != r.Epoch {
		return "", store.ErrFenced
	}
	if cur.HeadSHA == "" {
		return "", &failure{diagnose.Cause{Code: "no_changes", Owner: diagnose.Agent, Next: "The agent finished without changing a file."}}
	}
	return cur.HeadSHA, nil
}

// verify checks and previews the commit in a sandbox the agent was never in. It
// has no push capability and no credentials.
func (w *Worker) verify(ctx context.Context, r *store.Run, repo *config.Repo, image, head string) error {
	b, env, err := w.ready(ctx, r, repo, "verify", image, r.Branch(), head, "fetch", "egress")
	if err != nil {
		return err
	}
	conclusion, summary := "success", "passed"
	var failed *failure
	if repo.Check != "" {
		err := w.phase(ctx, r, b, "check", repo.Check, workdir, env, repo.CheckSecs)
		var f *failure
		if errors.As(err, &f) && f.Owner != diagnose.Platform {
			failed, conclusion, summary = f, "failure", f.Code+": "+strings.Join(f.Evidence, "; ")
		} else if err != nil {
			return err
		}
	}
	if err := w.Store.Revoke(ctx, b.name); err != nil {
		return err
	}
	var prev error
	if repo.Preview != "" {
		if prev = w.preview(ctx, r, repo, b); prev != nil {
			var f *failure
			if !errors.As(prev, &f) || f.Owner == diagnose.Platform {
				return prev
			}
		}
	}
	// The epoch is read again after the slow part and before anything public.
	if err := w.Store.Renew(ctx, r, w.Lease); err != nil {
		return err
	}
	pr, err := w.GitHub.EnsurePR(ctx, r.Repo, strings.TrimPrefix(r.Branch(), "refs/heads/"), repo.Base, r.Title,
		"Opened by an agent run. Merging needs a person and this repository's own checks.")
	if err != nil {
		return err
	}
	if err := w.Store.SetPR(ctx, r, pr); err != nil {
		return err
	}
	if err := w.GitHub.Check(ctx, r.Repo, head, "sandbox / clean checkout", conclusion, summary); err != nil {
		return err
	}
	if failed != nil {
		return failed
	}
	if prev != nil {
		return prev
	}
	return w.Store.Review(ctx, r, head, w.PreviewTTL)
}

func (w *Worker) preview(ctx context.Context, r *store.Run, repo *config.Repo, b *box) error {
	start := time.Now()
	_ = w.Store.SetPhase(ctx, b.name, "preview")
	// No token in this environment: the app has nothing left to ask for.
	env := map[string]string{"PORT": fmt.Sprint(repo.PreviewPort), "HOST": "0.0.0.0"}
	for k, v := range repo.Env {
		env[k] = v
	}
	if _, err := w.Provider.Exec(ctx, b.ref, provider.Cmd{Dir: workdir, Env: env, Timeout: 10 * time.Second,
		Shell: "(" + repo.Preview + "\n) >/tmp/preview.log 2>&1 </dev/null & echo $! >/tmp/preview.pid"}); err != nil {
		return w.lost(ctx, b, err)
	}
	// Asked through the sandbox's own outside address, so an app bound to
	// loopback fails here and not in front of a reviewer.
	probe := fmt.Sprintf(`ip=$(hostname -i | awk '{print $1}'); p=$(printf %%04X %[1]d)
kill -0 "$(cat /tmp/preview.pid)" 2>/dev/null && echo alive=1 || echo alive=0
awk -v p="$p" '$4=="0A"{split($2,a,":"); if(a[2]==p) print (a[1]=="0100007F"||a[1]=="00000000000000000000000001000000")?"listen=loopback":"listen=any"}' /proc/net/tcp /proc/net/tcp6 | sort | head -1
code=$(wget -T 3 -S -q -O /dev/null http://$ip:%[1]d/ 2>&1 | grep -o 'HTTP/[0-9.]* [0-9]*' | awk '{c=$2} END{print c+0}'); echo code=$code
tail -c 2000 /tmp/preview.log >&2`, repo.PreviewPort)
	f := diagnose.Facts{Phase: "preview", Exit: 1}
	deadline := start.Add(time.Duration(repo.PreviewSecs) * time.Second)
	for {
		res, err := w.Provider.Exec(ctx, b.ref, provider.Cmd{Shell: probe, Dir: "/", Timeout: 10 * time.Second})
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return w.lost(ctx, b, err)
		}
		f.Alive, f.Listen, f.HTTPCode, f.Err = strings.Contains(res.Out, "alive=1"), "", 0, res.Err
		for _, l := range strings.Fields(res.Out) {
			if v, ok := strings.CutPrefix(l, "listen="); ok && f.Listen != "any" {
				f.Listen = v
			}
			if v, ok := strings.CutPrefix(l, "code="); ok {
				_, _ = fmt.Sscan(v, &f.HTTPCode)
			}
		}
		if f.HTTPCode >= 200 && f.HTTPCode < 500 {
			break
		}
		if !f.Alive || time.Now().After(deadline) {
			obs.Phase.WithLabelValues("preview", "failed").Observe(time.Since(start).Seconds())
			c := diagnose.Explain(f)
			_ = w.Store.Event(ctx, r.ID, r.Epoch, "preview_failed", map[string]any{"cause": c})
			return &failure{c}
		}
		time.Sleep(300 * time.Millisecond)
	}
	obs.Phase.WithLabelValues("preview", "ok").Observe(time.Since(start).Seconds())
	ep, err := w.Provider.Endpoint(ctx, b.ref, repo.PreviewPort)
	if err != nil {
		return w.lost(ctx, b, err)
	}
	_ = w.Store.Event(ctx, r.ID, r.Epoch, "preview", map[string]any{"code": f.HTTPCode, "ms": time.Since(start).Milliseconds()})
	return w.Store.SetEndpoint(ctx, b.name, ep)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
