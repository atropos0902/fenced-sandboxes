// Package github is a small GitHub App client. Tokens are minted per
// repository and without the workflows permission, so a pushed change to
// .github/workflows is refused by GitHub itself.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type App struct {
	API, Git string // https://api.github.com, https://github.com
	ID       string
	Key      *rsa.PrivateKey
	HC       *http.Client

	mu     sync.Mutex
	tokens map[string]token
}

type token struct {
	val string
	exp time.Time
}

func New(api, git, id string, pemKey []byte) (*App, error) {
	b, _ := pem.Decode(pemKey)
	if b == nil {
		return nil, errors.New("app key is not PEM")
	}
	k, err := x509.ParsePKCS1PrivateKey(b.Bytes)
	if err != nil {
		a, err2 := x509.ParsePKCS8PrivateKey(b.Bytes)
		var ok bool
		if k, ok = a.(*rsa.PrivateKey); err2 != nil || !ok {
			return nil, errors.New("app key is not RSA")
		}
	}
	return &App{API: api, Git: git, ID: id, Key: k, HC: &http.Client{Timeout: 15 * time.Second}, tokens: map[string]token{}}, nil
}

func (a *App) jwt(now time.Time) (string, error) {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	s := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.ID})
	h := sha256.Sum256([]byte(s))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, h[:])
	return s + "." + base64.RawURLEncoding.EncodeToString(sig), err
}

// Token returns an installation token for one repository.
func (a *App) Token(ctx context.Context, repo string) (string, error) {
	a.mu.Lock()
	t, ok := a.tokens[repo]
	a.mu.Unlock()
	if ok && time.Until(t.exp) > 5*time.Minute {
		return t.val, nil
	}
	j, err := a.jwt(time.Now())
	if err != nil {
		return "", err
	}
	var inst struct{ ID int64 }
	if err := a.do(ctx, "GET", "/repos/"+repo+"/installation", "Bearer "+j, nil, &inst); err != nil {
		return "", fmt.Errorf("installation: %w", err)
	}
	var out struct {
		Token     string
		ExpiresAt time.Time `json:"expires_at"`
	}
	_, name, _ := strings.Cut(repo, "/")
	err = a.do(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), "Bearer "+j, map[string]any{
		"repositories": []string{name},
		"permissions":  map[string]string{"contents": "write", "pull_requests": "write", "checks": "write"},
	}, &out)
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	a.mu.Lock()
	a.tokens[repo] = token{out.Token, out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}

func (a *App) GitURL(ctx context.Context, repo string) (string, string, error) {
	t, err := a.Token(ctx, repo)
	return a.Git + "/" + repo + ".git", "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+t)), err
}

func (a *App) api(ctx context.Context, repo, method, path string, in, out any) error {
	t, err := a.Token(ctx, repo)
	if err != nil {
		return err
	}
	return a.do(ctx, method, "/repos/"+repo+path, "Bearer "+t, in, out)
}

type Error struct {
	Code int
	Body string
}

func (e *Error) Error() string { return fmt.Sprintf("GitHub HTTP %d: %s", e.Code, e.Body) }

func (a *App) do(ctx context.Context, method, path, auth string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return &Error{resp.StatusCode, string(b)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (a *App) BranchSHA(ctx context.Context, repo, branch string) (string, error) {
	var out struct{ Object struct{ SHA string } }
	err := a.api(ctx, repo, "GET", "/git/ref/heads/"+branch, nil, &out)
	return out.Object.SHA, err
}

// Blobs returns path to blob id at a commit. Lockfiles are identified by blob
// id, so the cache key never depends on anything a sandbox computed.
func (a *App) Blobs(ctx context.Context, repo, sha string) (map[string]string, error) {
	var out struct {
		Truncated bool
		Tree      []struct{ Path, Type, SHA string }
	}
	if err := a.api(ctx, repo, "GET", "/git/trees/"+sha+"?recursive=1", nil, &out); err != nil {
		return nil, err
	}
	if out.Truncated {
		return nil, errors.New("tree listing was truncated")
	}
	m := map[string]string{}
	for _, t := range out.Tree {
		if t.Type == "blob" {
			m[t.Path] = t.SHA
		}
	}
	return m, nil
}

// EnsurePR is safe to call again for the same branch.
func (a *App) EnsurePR(ctx context.Context, repo, head, base, title, body string) (int, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var found []struct{ Number int }
	q := url.Values{"head": {owner + ":" + head}, "state": {"open"}}
	if err := a.api(ctx, repo, "GET", "/pulls?"+q.Encode(), nil, &found); err != nil {
		return 0, err
	}
	if len(found) > 0 {
		return found[0].Number, nil
	}
	var pr struct{ Number int }
	err := a.api(ctx, repo, "POST", "/pulls", map[string]any{"title": title, "head": head, "base": base, "body": body}, &pr)
	return pr.Number, err
}

// Check reports on one commit, never on a branch.
func (a *App) Check(ctx context.Context, repo, sha, name, conclusion, summary string) error {
	return a.api(ctx, repo, "POST", "/check-runs", map[string]any{
		"name": name, "head_sha": sha, "status": "completed", "conclusion": conclusion,
		"output": map[string]string{"title": name, "summary": summary},
	}, nil)
}
