package gate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fenced/internal/config"
	"fenced/internal/fakegh"
	"fenced/internal/store"
)

// These tests push with the real git client, through the gate, into real
// git-http-backend. They need Postgres: TEST_DATABASE_URL.

type up struct{ url string }

func (u up) GitURL(_ context.Context, repo string) (string, string, error) {
	return u.url + "/" + repo + ".git", "Basic " + b64("x-access-token:"+fakegh.InstallationToken), nil
}

type world struct {
	t    *testing.T
	st   *store.Store
	gh   *fakegh.Server
	gate *httptest.Server
	g    *Gate
	cfg  *config.Config
}

func setup(t *testing.T) *world {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.DB.Close)
	if _, err := st.DB.Exec(ctx, `TRUNCATE runs, sandboxes, events, snapshots CASCADE`); err != nil {
		t.Fatal(err)
	}
	gh := &fakegh.Server{Root: t.TempDir(), Backend: "/usr/lib/git-core/git-http-backend", LLMKey: "real-model-key"}
	seed := t.TempDir()
	_ = os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644)
	if err := gh.Seed("demo/shop", seed); err != nil {
		t.Fatal(err)
	}
	ghs := httptest.NewServer(gh.Handler())
	t.Cleanup(ghs.Close)
	cfg, err := config.Parse([]byte(`{"repos":[{"name":"demo/shop","base":"main","image":"x","allow_hosts":["ok.example"],
		"creds":[{"name":"llm","base":"` + ghs.URL + `/llm","header":"X-Api-Key","secret_env":"TEST_LLM_KEY"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_LLM_KEY", "real-model-key")
	g := &Gate{Store: st, Config: cfg, Up: up{ghs.URL}, HC: http.DefaultClient, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	gs := httptest.NewServer(g.Handler())
	t.Cleanup(gs.Close)
	return &world{t, st, gh, gs, g, cfg}
}

func (w *world) start() *store.Run {
	w.t.Helper()
	if _, err := w.st.CreateRun(context.Background(), "demo/shop", "t", "true"); err != nil {
		w.t.Fatal(err)
	}
	return w.claim(time.Minute)
}

func (w *world) claim(lease time.Duration) *store.Run {
	w.t.Helper()
	r, err := w.st.Claim(context.Background(), "w", lease)
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *world) expire(r *store.Run) {
	w.t.Helper()
	if _, err := w.st.DB.Exec(context.Background(), `UPDATE runs SET claim_until=now()-interval '1 second' WHERE id=$1`, r.ID); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) token(r *store.Run, caps ...string) string {
	w.t.Helper()
	_, tok, err := w.st.Intend(context.Background(), r, "agent", "test", caps)
	if err != nil {
		w.t.Fatal(err)
	}
	return tok
}

type checkout struct {
	t        *testing.T
	dir, tok string
}

func (w *world) clone(tok string) *checkout {
	w.t.Helper()
	c := &checkout{w.t, filepath.Join(w.t.TempDir(), "repo"), tok}
	if out, err := c.git("clone", "-q", w.gate.URL+"/git/demo/shop.git", c.dir); err != nil {
		w.t.Fatalf("clone: %s", out)
	}
	return c
}

func (c *checkout) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if _, err := os.Stat(c.dir); err == nil {
		cmd.Dir = c.dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+b64("run:"+c.tok),
		"GIT_CONFIG_KEY_1=user.name", "GIT_CONFIG_VALUE_1=t", "GIT_CONFIG_KEY_2=user.email", "GIT_CONFIG_VALUE_2=t@localhost")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *checkout) commit(name string, size int) string {
	c.t.Helper()
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*7919 ^ i>>8)
	}
	_ = os.WriteFile(filepath.Join(c.dir, name), b, 0o644)
	_, _ = c.git("add", "-A")
	if out, err := c.git("commit", "-q", "-m", name); err != nil {
		c.t.Fatal(out)
	}
	out, _ := c.git("rev-parse", "HEAD")
	return strings.TrimSpace(out)
}

func (w *world) upstream(ref string) string {
	out, _ := exec.Command("git", "--git-dir", filepath.Join(w.gh.Root, "demo/shop.git"), "rev-parse", "-q", "--verify", ref).Output()
	return strings.TrimSpace(string(out))
}

func TestPushReachesOnlyTheRunsOwnBranch(t *testing.T) {
	w := setup(t)
	r := w.start()
	c := w.clone(w.token(r, "fetch", "push"))
	main := w.upstream("refs/heads/main")
	sha := c.commit("a", 10)
	for _, ref := range []string{"refs/heads/main", "refs/heads/agent/other", "refs/tags/v1", "refs/heads/agent/" + r.ID + "/x"} {
		if out, err := c.git("push", "-q", "origin", "HEAD:"+ref); err == nil {
			t.Fatalf("push to %s went through: %s", ref, out)
		}
		if ref != "refs/heads/main" && w.upstream(ref) != "" {
			t.Fatalf("%s exists upstream", ref)
		}
	}
	if w.upstream("refs/heads/main") != main {
		t.Fatal("main moved")
	}
	// One good ref does not carry a bad one through with it.
	if out, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch(), "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("mixed push went through: %s", out)
	}
	if out, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch()); err != nil {
		t.Fatalf("own branch: %s", out)
	}
	if w.upstream(r.Branch()) != sha {
		t.Fatal("upstream is not at the pushed commit")
	}
	if out, err := c.git("push", "-q", "origin", ":"+r.Branch()); err == nil {
		t.Fatalf("delete went through: %s", out)
	}
	if w.upstream(r.Branch()) != sha {
		t.Fatal("the branch was deleted upstream")
	}
	if got, _ := w.st.GetRun(context.Background(), r.ID); got.HeadSHA != sha {
		t.Fatalf("recorded head %q, want %q", got.HeadSHA, sha)
	}
}

func TestPushFromAnUnrecordedHeadIsRefused(t *testing.T) {
	w := setup(t)
	r := w.start()
	c := w.clone(w.token(r, "fetch", "push"))
	first := c.commit("a", 10)
	if out, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch()); err != nil {
		t.Fatal(out)
	}
	if _, err := w.st.DB.Exec(context.Background(), `UPDATE runs SET head_sha=$2 WHERE id=$1`, r.ID, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	c.commit("b", 10)
	if _, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch()); err == nil || w.upstream(r.Branch()) != first {
		t.Fatal("pushed over a head we had not recorded")
	}
}

func TestPushLimits(t *testing.T) {
	w := setup(t)
	w.g.MaxPush = 8 << 10
	r := w.start()
	tok := w.token(r, "fetch", "push")
	c := w.clone(tok)
	c.commit("big", 4<<20)
	if _, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch()); err == nil || w.upstream(r.Branch()) != "" {
		t.Fatal("push over the size limit went through")
	}
	req, _ := http.NewRequest("POST", w.gate.URL+"/git/demo/shop.git/git-receive-pack", strings.NewReader("0000"))
	req.Header.Set("Authorization", "Basic "+b64("run:"+tok))
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("encoded push: %d", resp.StatusCode)
	}
}

func TestLargePush(t *testing.T) {
	w := setup(t)
	r := w.start()
	c := w.clone(w.token(r, "fetch", "push"))
	sha := c.commit("big", 6<<20) // over http.postBuffer, so git probes and then chunks
	if out, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch()); err != nil {
		t.Fatal(out)
	}
	if w.upstream(r.Branch()) != sha {
		t.Fatal("not upstream")
	}
}

func TestReplacedAttemptCannotPush(t *testing.T) {
	w := setup(t)
	r1 := w.start()
	old := w.clone(w.token(r1, "fetch", "push"))
	old.commit("stale", 10)
	w.expire(r1)
	r2 := w.claim(time.Minute)
	if r2 == nil || r2.ID != r1.ID || r2.Epoch != 2 {
		t.Fatalf("takeover: %+v", r2)
	}
	if out, err := old.git("push", "-q", "origin", "HEAD:"+r1.Branch()); err == nil {
		t.Fatalf("stale push went through: %s", out)
	}
	if out, err := old.git("fetch", "-q"); err == nil {
		t.Fatalf("stale fetch went through: %s", out)
	}
	cur := w.clone(w.token(r2, "fetch", "push"))
	sha := cur.commit("new", 10)
	if out, err := cur.git("push", "-q", "origin", "HEAD:"+r2.Branch()); err != nil {
		t.Fatal(out)
	}
	if w.upstream(r2.Branch()) != sha {
		t.Fatal("new attempt's commit is not upstream")
	}
	if err := w.st.Review(context.Background(), r1, sha, time.Minute); err != store.ErrFenced {
		t.Fatalf("old worker moved the run on: %v", err)
	}
}

// The hard case. The token was good when the push arrived, and the run is taken
// over while the push is under way. It has to finish before or not at all.
func TestTakeoverWaitsForAPushInProgress(t *testing.T) {
	w := setup(t)
	r1 := w.start()
	c := w.clone(w.token(r1, "fetch", "push"))
	sha := c.commit("a", 10)
	w.expire(r1) // the lease is already over, so only the push is in the way
	entered, release := make(chan struct{}), make(chan struct{})
	var once, open sync.Once
	letGo := func() { open.Do(func() { close(release) }) }
	defer letGo()
	w.gh.BeforeReceive = func(http.ResponseWriter) bool {
		once.Do(func() { close(entered); <-release })
		return false
	}
	pushed := make(chan error, 1)
	go func() {
		out, err := c.git("push", "-q", "origin", "HEAD:"+r1.Branch())
		if err != nil {
			err = fmt.Errorf("%s", out)
		}
		pushed <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Passing over the run matters as much as not taking it. A claim that waits
	// on this row keeps a worker from every other run in the queue.
	if r, err := w.st.Claim(ctx, "w2", time.Minute); r != nil || err != nil {
		letGo()
		<-pushed
		t.Fatalf("claim during a push: %+v %v", r, err)
	}
	letGo()
	if err := <-pushed; err != nil {
		t.Fatal(err)
	}
	r2 := w.claim(time.Minute)
	if r2 == nil || r2.HeadSHA != sha {
		t.Fatalf("the next attempt does not know about the push: %+v", r2)
	}
}

func TestTakeoverBetweenUploadAndLock(t *testing.T) {
	w := setup(t)
	r1 := w.start()
	c := w.clone(w.token(r1, "fetch", "push"))
	c.commit("a", 10)
	ctx := context.Background()
	// Hold the row so the push has passed its token check and is waiting, then
	// raise the epoch in the same transaction it is waiting for.
	tx, err := w.st.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, r1.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.git("push", "-q", "origin", "HEAD:"+r1.Branch()); done <- err }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := w.st.DB.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND datname=current_database()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("push never reached the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET epoch=epoch+1 WHERE id=$1`, r1.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("push went through after the epoch was raised")
	}
	if w.upstream(r1.Branch()) != "" {
		t.Fatal("stale commit is upstream")
	}
}

func TestPushThatLandedButLostItsAnswer(t *testing.T) {
	w := setup(t)
	r := w.start()
	c := w.clone(w.token(r, "fetch", "push"))
	sha := c.commit("a", 10)
	w.gh.AfterReceive = func(rw http.ResponseWriter) bool {
		conn, _, _ := rw.(http.Hijacker).Hijack()
		conn.Close()
		return true
	}
	_, _ = c.git("push", "-q", "origin", "HEAD:"+r.Branch())
	got, _ := w.st.GetRun(context.Background(), r.ID)
	if w.upstream(r.Branch()) != sha || got.HeadSHA != sha || got.State != "running" {
		t.Fatalf("upstream %q recorded %q state %s", w.upstream(r.Branch()), got.HeadSHA, got.State)
	}
}

func TestPushWithUnknownOutcomeStopsTheRun(t *testing.T) {
	w := setup(t)
	r := w.start()
	tok := w.token(r, "fetch", "push")
	c := w.clone(tok)
	c.commit("a", 10)
	w.gh.BeforeReceive = func(rw http.ResponseWriter) bool {
		conn, _, _ := rw.(http.Hijacker).Hijack()
		conn.Close()
		return true
	}
	if _, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch()); err == nil {
		t.Fatal("push reported success")
	}
	got, _ := w.st.GetRun(context.Background(), r.ID)
	if got.State != "attention" || got.Code != "push_unknown" {
		t.Fatalf("state %s code %s", got.State, got.Code)
	}
	if _, err := w.st.Grant(context.Background(), tok); err != store.ErrFenced {
		t.Fatal("token still works")
	}
}

func TestCapabilitiesAndCancel(t *testing.T) {
	w := setup(t)
	r := w.start()
	tok := w.token(r, "fetch")
	c := w.clone(tok)
	c.commit("a", 10)
	if _, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch()); err == nil {
		t.Fatal("fetch-only token pushed")
	}
	if code, _ := get(w.gate.URL+"/cred/llm/v1", "X-Api-Key", tok); code != 403 {
		t.Fatalf("cred without capability: %d", code)
	}
	if code, _ := get(w.gate.URL+"/git/demo/other.git/info/refs?service=git-upload-pack", "Authorization", "Basic "+b64("run:"+tok)); code != 403 {
		t.Fatalf("other repository: %d", code)
	}
	if err := w.st.Cancel(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.git("fetch", "-q"); err == nil {
		t.Fatal("token outlived the cancel")
	}
}

func TestCredentialIsSwappedAndNotReturned(t *testing.T) {
	w := setup(t)
	r := w.start()
	tok := w.token(r, "cred")
	code, body := get(w.gate.URL+"/cred/llm/v1/messages", "X-Api-Key", tok)
	if code != 200 || !strings.Contains(body, "/llm/v1/messages") {
		t.Fatalf("%d %s", code, body)
	}
	if len(w.gh.LLMSeen) != 1 || w.gh.LLMSeen[0] != "real-model-key" {
		t.Fatalf("upstream saw %v", w.gh.LLMSeen)
	}
	if code, _ := get(w.gate.URL+"/cred/nope/x", "X-Api-Key", tok); code != 403 {
		t.Fatalf("unknown credential: %d", code)
	}
}

func TestApproveIsBoundToTheCommit(t *testing.T) {
	w := setup(t)
	ctx := context.Background()
	r := w.start()
	c := w.clone(w.token(r, "fetch", "push"))
	sha := c.commit("a", 10)
	if out, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch()); err != nil {
		t.Fatal(out)
	}
	if err := w.st.Review(ctx, r, strings.Repeat("a", 40), time.Minute); err != store.ErrFenced {
		t.Fatalf("review at another commit: %v", err)
	}
	if err := w.st.Review(ctx, r, sha, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := c.git("push", "-q", "origin", "HEAD~0:"+r.Branch()); err == nil {
		c.commit("b", 10)
		if _, err := c.git("push", "-q", "origin", "HEAD:"+r.Branch()); err == nil {
			t.Fatal("branch moved while it was being reviewed")
		}
	}
	if err := w.st.Approve(ctx, r.ID, strings.Repeat("a", 40)); err != store.ErrConflict {
		t.Fatalf("approve of another commit: %v", err)
	}
	if err := w.st.Approve(ctx, r.ID, sha); err != nil {
		t.Fatal(err)
	}
}

func TestEgress(t *testing.T) {
	w := setup(t)
	r := w.start()
	tok := w.token(r, "egress")
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	addr := "93.184.216.34"
	e := &Egress{Store: w.st, Config: w.cfg, Log: slog.Default(), Recheck: 50 * time.Millisecond,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr(addr)}, nil
		},
		Dial: func(ctx context.Context, n, a string) (net.Conn, error) {
			if a != "93.184.216.34:443" {
				t.Errorf("dialed %s", a)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", echo.Addr().String())
		}}
	ps := httptest.NewServer(e)
	defer ps.Close()
	connect := func(host, tok string) (net.Conn, string) {
		c, err := net.Dial("tcp", ps.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", host, host, b64("run:"+tok))
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		b := make([]byte, 12)
		_, _ = io.ReadFull(c, b)
		return c, strings.TrimSpace(string(b[9:]))
	}
	for _, tc := range []struct{ host, tok, want string }{
		{"evil.example:443", tok, "403"}, {"ok.example:22", tok, "403"}, {"ok.example.evil.example:443", tok, "403"},
		{"169.254.169.254:443", tok, "403"}, {"ok.example:443", "wrong", "403"},
	} {
		c, got := connect(tc.host, tc.tok)
		c.Close()
		if got != tc.want {
			t.Errorf("%s: %s, want %s", tc.host, got, tc.want)
		}
	}
	plain := func(tok string) string {
		c, err := net.Dial("tcp", ps.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprintf(c, "GET http://ok.example:443/ HTTP/1.1\r\nHost: ok.example:443\r\nProxy-Authorization: Basic %s\r\n\r\n", b64("run:"+tok))
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		b := make([]byte, 12)
		_, _ = io.ReadFull(c, b)
		return strings.TrimSpace(string(b[9:]))
	}
	if got := plain(tok); got != "403" {
		t.Errorf("plain request: %s", got)
	}
	_, nocap, err := w.st.Intend(context.Background(), r, "verify", "test", []string{"fetch"})
	if err != nil {
		t.Fatal(err)
	}
	if c, got := connect("ok.example:443", nocap); got != "403" {
		t.Errorf("no egress capability: %s", got)
	} else {
		c.Close()
	}
	// An allowed name that answers with an internal address is still refused.
	addr = "169.254.169.254"
	if c, got := connect("ok.example:443", tok); got != "403" {
		t.Errorf("rebinding: %s", got)
	} else {
		c.Close()
	}
	addr = "93.184.216.34"
	c, got := connect("ok.example:443", tok)
	defer c.Close()
	if got != "200" {
		t.Fatalf("allowed host: %s", got)
	}
	w.expire(r)
	w.claim(time.Minute)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("tunnel outlived the attempt: %v", err)
	}
	hosts, _ := w.st.Denied(context.Background(), r.ID, r.Epoch, store.SandboxName(r.ID, r.Epoch, "agent"), "provision")
	if !strings.Contains(strings.Join(hosts, " "), "evil.example") {
		t.Errorf("denied hosts not recorded: %v", hosts)
	}
}

func get(u, h, v string) (int, string) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set(h, v)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
