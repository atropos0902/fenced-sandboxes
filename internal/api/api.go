// Package api is the operator-facing HTTP API. It runs in the control process,
// which no sandbox shares a network with.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"fenced/internal/config"
	"fenced/internal/gate"
	"fenced/internal/store"
)

type API struct {
	Store      *store.Store
	Config     *config.Config
	Token      string
	PreviewKey []byte
	PreviewURL string // e.g. http://%s.preview.localhost:8091
}

func (a *API) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /runs", a.create)
	m.HandleFunc("GET /runs/{id}", a.get)
	m.HandleFunc("GET /runs/{id}/events", a.events)
	m.HandleFunc("POST /runs/{id}/cancel", a.cancel)
	m.HandleFunc("POST /runs/{id}/approve", a.approve)
	root := http.NewServeMux()
	root.Handle("GET /metrics", promhttp.Handler())
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Store.DB.Ping(r.Context()); err != nil {
			http.Error(w, "database", http.StatusServiceUnavailable)
		}
	})
	root.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if a.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		m.ServeHTTP(w, r)
	}))
	return root
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, store.ErrConflict):
		code = http.StatusConflict
	}
	reply(w, code, map[string]string{"error": err.Error()})
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	var in struct{ Repo, Title, Agent string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || in.Agent == "" || in.Title == "" {
		reply(w, 400, map[string]string{"error": "repo, title and agent are required"})
		return
	}
	if a.Config.Repo(in.Repo) == nil {
		reply(w, 400, map[string]string{"error": "unknown repo"})
		return
	}
	run, err := a.Store.CreateRun(r.Context(), in.Repo, in.Title, in.Agent)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 201, run)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	run, err := a.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	out := struct {
		*store.Run
		Preview string `json:"preview,omitempty"`
	}{Run: run}
	if run.State == "review" {
		out.Preview = fmt.Sprintf(a.PreviewURL, run.ID) + "/?preview_token=" + gate.Sign(a.PreviewKey, run.ID, run.HeadSHA, time.Now().Add(time.Hour))
	}
	reply(w, 200, out)
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	ev, err := a.Store.Events(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, ev)
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.Cancel(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func (a *API) approve(w http.ResponseWriter, r *http.Request) {
	var in struct{ SHA string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		reply(w, 400, map[string]string{"error": "sha is required"})
		return
	}
	if err := a.Store.Approve(r.Context(), r.PathValue("id"), in.SHA); err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
