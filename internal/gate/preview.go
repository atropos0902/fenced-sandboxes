package gate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fenced/internal/provider"
	"fenced/internal/store"
)

// Preview serves a run's app at <run>.<domain>. Each run has its own origin, so
// one preview's script cannot read another's cookies or call our API as the
// viewer.
type Preview struct {
	Store  *store.Store
	Domain string
	Key    []byte
}

const cookie = "fenced_preview"

func Sign(key []byte, run, head string, exp time.Time) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s|%s|%d", run, head, exp.Unix())
	return strconv.FormatInt(exp.Unix(), 10) + "." + hex.EncodeToString(m.Sum(nil))
}

// valid ties a link to one commit. A new head needs a new link, so nobody
// reviews a tab that quietly moved on.
func valid(key []byte, run, head, tok string, now time.Time) bool {
	s, _, ok := strings.Cut(tok, ".")
	n, err := strconv.ParseInt(s, 10, 64)
	if !ok || err != nil || now.Unix() > n {
		return false
	}
	return hmac.Equal([]byte(tok), []byte(Sign(key, run, head, time.Unix(n, 0))))
}

func (p *Preview) lookup(ctx context.Context, run string) (provider.Endpoint, string, error) {
	var raw []byte
	var head string
	var ep provider.Endpoint
	err := p.Store.DB.QueryRow(ctx, `
		SELECT s.endpoint, r.head_sha FROM runs r JOIN sandboxes s ON s.run_id=r.id AND s.epoch=r.epoch AND s.role='verify'
		WHERE r.id=$1 AND r.state='review' AND r.review_until>now() AND s.state='live' AND s.endpoint IS NOT NULL`, run).Scan(&raw, &head)
	if err != nil {
		return ep, "", err
	}
	return ep, head, json.Unmarshal(raw, &ep)
}

func (p *Preview) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := strings.Cut(r.Host, ":")
	run, ok := strings.CutSuffix(host, "."+p.Domain)
	if !ok || strings.Contains(run, ".") {
		http.NotFound(w, r)
		return
	}
	ep, head, err := p.lookup(r.Context(), run)
	if err != nil {
		http.Error(w, "This preview is not running.", http.StatusGone)
		return
	}
	if t := r.URL.Query().Get("preview_token"); t != "" && valid(p.Key, run, head, t, time.Now()) {
		http.SetCookie(w, &http.Cookie{Name: cookie, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		u := *r.URL
		q := u.Query()
		q.Del("preview_token")
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
		return
	}
	c, err := r.Cookie(cookie)
	if err != nil || !valid(p.Key, run, head, c.Value, time.Now()) {
		http.Error(w, "This preview link has expired or belongs to an older commit.", http.StatusForbidden)
		return
	}
	base, err := url.Parse(ep.URL)
	if err != nil {
		http.Error(w, "bad endpoint", http.StatusBadGateway)
		return
	}
	(&httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Header.Del("Cookie")
			for _, c := range pr.In.Cookies() {
				if c.Name != cookie { // the app must not be able to take the viewer's link
					pr.Out.AddCookie(c)
				}
			}
			for k, v := range ep.Headers {
				pr.Out.Header.Set(k, v)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// Cookies stay on this run's own host whatever the app asks for.
			set := resp.Cookies()
			resp.Header.Del("Set-Cookie")
			for _, c := range set {
				if c.Name != cookie {
					c.Domain = ""
					resp.Header.Add("Set-Cookie", c.String())
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "The app in this preview did not answer.", http.StatusBadGateway)
		},
	}).ServeHTTP(w, r)
}
