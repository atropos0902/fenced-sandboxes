// Package fakegh is enough of GitHub to run the system without an account: the
// App token exchange, refs, trees, pull requests, check runs and real git over
// HTTP. It refuses git without the token it issued.
package fakegh

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const InstallationToken = "ghs_real_installation_token"

type Check struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Conclusion string `json:"conclusion"`
	Output     any    `json:"output"`
}

type PR struct {
	Number int    `json:"number"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	Title  string `json:"title"`
}

type Server struct {
	Root    string // directory of owner/repo.git bare repositories
	Backend string // path of git-http-backend
	LLMKey  string

	// BeforeReceive and AfterReceive let tests stop or break a push midway.
	BeforeReceive, AfterReceive func(w http.ResponseWriter) (stop bool)

	mu       sync.Mutex
	PRs      []PR
	Checks   []Check
	TokenReq map[string]any
	LLMSeen  []string
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /repos/{o}/{r}/installation", func(w http.ResponseWriter, r *http.Request) {
		if strings.Count(r.Header.Get("Authorization"), ".") != 2 {
			http.Error(w, "want a JWT", 401)
			return
		}
		j(w, 200, map[string]int{"id": 77})
	})
	m.HandleFunc("POST /app/installations/77/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		s.TokenReq = in
		s.mu.Unlock()
		j(w, 201, map[string]any{"token": InstallationToken, "expires_at": time.Now().Add(time.Hour)})
	})
	api := func(pat string, h http.HandlerFunc) {
		m.HandleFunc(pat, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+InstallationToken {
				http.Error(w, "bad token", 401)
				return
			}
			h(w, r)
		})
	}
	api("GET /repos/{o}/{r}/git/ref/heads/{b...}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.git(r, "rev-parse", "refs/heads/"+r.PathValue("b"))
		if err != nil {
			http.Error(w, "no such ref", 404)
			return
		}
		j(w, 200, map[string]any{"object": map[string]string{"sha": strings.TrimSpace(out)}})
	})
	api("GET /repos/{o}/{r}/git/trees/{sha}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.git(r, "ls-tree", "-r", r.PathValue("sha"))
		if err != nil {
			http.Error(w, "no such tree", 404)
			return
		}
		tree := []map[string]string{}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if f := strings.Fields(l); len(f) == 4 {
				tree = append(tree, map[string]string{"type": f[1], "sha": f[2], "path": f[3]})
			}
		}
		j(w, 200, map[string]any{"tree": tree})
	})
	api("GET /repos/{o}/{r}/pulls", func(w http.ResponseWriter, r *http.Request) {
		_, head, _ := strings.Cut(r.URL.Query().Get("head"), ":")
		out := []PR{}
		s.mu.Lock()
		for _, p := range s.PRs {
			if p.Head == head {
				out = append(out, p)
			}
		}
		s.mu.Unlock()
		j(w, 200, out)
	})
	api("POST /repos/{o}/{r}/pulls", func(w http.ResponseWriter, r *http.Request) {
		var p PR
		_ = json.NewDecoder(r.Body).Decode(&p)
		s.mu.Lock()
		p.Number = len(s.PRs) + 1
		s.PRs = append(s.PRs, p)
		s.mu.Unlock()
		j(w, 201, p)
	})
	api("POST /repos/{o}/{r}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		var c Check
		_ = json.NewDecoder(r.Body).Decode(&c)
		s.mu.Lock()
		s.Checks = append(s.Checks, c)
		s.mu.Unlock()
		j(w, 201, c)
	})
	m.HandleFunc("/llm/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("X-Api-Key")
		s.mu.Lock()
		s.LLMSeen = append(s.LLMSeen, k)
		s.mu.Unlock()
		if k != s.LLMKey {
			http.Error(w, "bad key", 401)
			return
		}
		j(w, 200, map[string]string{"path": r.URL.Path, "answer": "ok"})
	})
	m.HandleFunc("GET /_state", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		j(w, 200, map[string]any{"prs": s.PRs, "checks": s.Checks, "token_request": s.TokenReq, "llm_seen": s.LLMSeen})
	})
	back := &cgi.Handler{Path: s.Backend, Env: []string{"GIT_PROJECT_ROOT=" + s.Root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=app"}, InheritEnv: []string{"PATH"}}
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, p, ok := r.BasicAuth(); !ok || p != InstallationToken {
			w.Header().Set("WWW-Authenticate", "Basic")
			http.Error(w, "auth", 401)
			return
		}
		recv := strings.HasSuffix(r.URL.Path, "/git-receive-pack")
		if recv && s.BeforeReceive != nil && s.BeforeReceive(w) {
			return
		}
		if recv && s.AfterReceive != nil {
			back.ServeHTTP(&discard{h: http.Header{}}, r)
			if s.AfterReceive(w) {
				return
			}
		}
		back.ServeHTTP(w, r)
	})
	return m
}

type discard struct{ h http.Header }

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) WriteHeader(int)             {}

func (s *Server) git(r *http.Request, args ...string) (string, error) {
	dir := filepath.Join(s.Root, r.PathValue("o"), r.PathValue("r")+".git")
	out, err := exec.Command("git", append([]string{"-c", "safe.directory=*", "--git-dir", dir}, args...)...).Output()
	return string(out), err
}

// Seed creates a bare repository whose main branch holds dir's files.
func (s *Server) Seed(repo, dir string) error {
	bare := filepath.Join(s.Root, repo+".git")
	sh := fmt.Sprintf(`set -e
git init -q --bare -b main %[1]q
git --git-dir %[1]q config http.receivepack true
t=$(mktemp -d); cp -r %[2]q/. "$t"; cd "$t"
git init -q -b main; git add -A
git -c user.name=seed -c user.email=seed@localhost commit -q -m seed
git push -q %[1]q main`, bare, dir)
	out, err := exec.Command("sh", "-c", sh).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func j(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
