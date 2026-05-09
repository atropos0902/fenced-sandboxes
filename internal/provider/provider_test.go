package provider

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The E2B and Blaxel adapters are tested against servers written from their
// published API descriptions. Nothing here has called the live services.

func frame(flag byte, s string) []byte {
	b := make([]byte, 5, 5+len(s))
	b[0] = flag
	binary.BigEndian.PutUint32(b[1:], uint32(len(s)))
	return append(b, s...)
}

func TestE2B(t *testing.T) {
	var created map[string]any
	var killed []string
	listed := ""
	mode := "exit"
	m := http.NewServeMux()
	m.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			http.Error(w, "", 401)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&created)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"sandboxID":"sb1","templateID":"t","clientID":"c","envdVersion":"1","envdAccessToken":"envd","trafficAccessToken":"traffic","domain":"e2b.test"}`)
	})
	m.HandleFunc("GET /v2/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		listed = r.URL.Query().Get("state")
		if strings.HasPrefix(r.URL.Query().Get("metadata"), "name=") {
			_, _ = io.WriteString(w, `[{"sandboxID":"dup","metadata":{"owner":"me","name":"n1"},"startedAt":"0001-01-01T00:00:00Z"}]`)
			return
		}
		if r.URL.Query().Get("nextToken") == "" {
			w.Header().Set("X-Next-Token", "p2")
			_, _ = io.WriteString(w, `[{"sandboxID":"a","metadata":{"owner":"me","name":"n1"},"startedAt":"0001-01-01T00:00:00Z"},{"sandboxID":"x","metadata":{"owner":"someone"}}]`)
			return
		}
		_, _ = io.WriteString(w, `[{"sandboxID":"b","metadata":{"owner":"me","name":"n2"},"startedAt":"0001-01-01T00:00:00Z"}]`)
	})
	m.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		killed = append(killed, r.PathValue("id"))
		if r.PathValue("id") == "gone" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(204)
	})
	m.HandleFunc("POST /process.Process/Start", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != "envd" || r.Header.Get("Content-Type") != "application/connect+json" {
			http.Error(w, "", 401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if int(binary.BigEndian.Uint32(b[1:5])) != len(b)-5 {
			t.Error("bad envelope")
		}
		_, _ = w.Write(frame(0, `{"event":{"start":{"pid":7}}}`))
		_, _ = w.Write(frame(0, `{"event":{"data":{"stdout":"aGk="}}}`))
		_, _ = w.Write(frame(0, `{"event":{"data":{"stderr":"b29wcw=="}}}`))
		switch mode {
		case "exit":
			_, _ = w.Write(frame(0, `{"event":{"end":{"exitCode":3,"exited":true,"status":"exit status 3"}}}`))
		case "signal":
			_, _ = w.Write(frame(0, `{"event":{"end":{"exited":false,"status":"signal: killed"}}}`))
		case "error":
			_, _ = w.Write(frame(2, `{"error":{"code":"unavailable"}}`))
		}
	})
	s := httptest.NewServer(m)
	defer s.Close()
	e := NewE2B("k", "me", []string{"203.0.113.7/32"})
	e.API, e.EnvdURL = s.URL, func(string, string) string { return s.URL }
	ctx := context.Background()
	ref, err := e.Create(ctx, Spec{Name: "n1", RunID: "r", Epoch: 2, Image: "tpl", TTL: time.Hour})
	if err != nil || ref != "sb1" {
		t.Fatal(ref, err)
	}
	if len(killed) != 1 || killed[0] != "dup" {
		t.Fatalf("earlier create was not removed: %v", killed)
	}
	net := created["network"].(map[string]any)
	if created["secure"] != true || created["autoPause"] != false || net["allowPublicTraffic"] != false || net["denyOut"].([]any)[0] != "0.0.0.0/0" {
		t.Fatalf("created open: %v", created)
	}
	r, err := e.Exec(ctx, ref, Cmd{Shell: "x", Timeout: time.Minute})
	if err != nil || r.Exit != 3 || r.Out != "hi" || r.Err != "oops" {
		t.Fatalf("%+v %v", r, err)
	}
	mode = "signal"
	if r, err := e.Exec(ctx, ref, Cmd{Shell: "x"}); err != nil || r.Exit == 0 {
		t.Fatalf("a killed process looked successful: %+v %v", r, err)
	}
	for _, mode = range []string{"error", "cut"} {
		if _, err := e.Exec(ctx, ref, Cmd{Shell: "x"}); err == nil {
			t.Fatalf("%s: no error", mode)
		}
	}
	l, err := e.List(ctx)
	if err != nil || len(l) != 2 || !strings.Contains(listed, "paused") {
		t.Fatalf("%v %v %q", l, err, listed)
	}
	ep, _ := e.Endpoint(ctx, ref, 3000)
	if ep.URL != "https://3000-sb1.e2b.test" || ep.Headers["E2b-Traffic-Access-Token"] != "traffic" {
		t.Fatalf("%+v", ep)
	}
	if err := e.Destroy(ctx, "n", "gone"); err != nil {
		t.Fatal("already gone should be fine:", err)
	}
}

func TestBlaxel(t *testing.T) {
	var created map[string]any
	polls := 0
	var s *httptest.Server
	m := http.NewServeMux()
	m.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("createIfNotExist") != "true" || r.Header.Get("X-Blaxel-Authorization") != "Bearer k" {
			http.Error(w, "", 400)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&created)
		_, _ = io.WriteString(w, `{"metadata":{"name":"n1"}}`)
	})
	m.HandleFunc("GET /sandboxes/n1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"metadata":{"name":"n1","url":"`+s.URL+`"}}`)
	})
	m.HandleFunc("POST /process", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"running"}`)
	})
	m.HandleFunc("GET /process/{n}", func(w http.ResponseWriter, _ *http.Request) {
		polls++
		_, _ = io.WriteString(w, `{"status":"failed","exitCode":4,"stdout":"o","stderr":"e"}`)
	})
	m.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			_, _ = io.WriteString(w, `{"data":[{"metadata":{"name":"a","labels":{"owner":"me"}}},{"metadata":{"name":"b","labels":{"owner":"me-too"}}}],"meta":{"hasMore":true,"nextCursor":"c"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"metadata":{"name":"c","labels":{"owner":"me"}}}],"meta":{"hasMore":false}}`)
	})
	m.HandleFunc("DELETE /sandboxes/{n}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	s = httptest.NewServer(m)
	defer s.Close()
	b := NewBlaxel("k", "ws", "me", "gate.example")
	b.API = s.URL
	ctx := context.Background()
	if _, err := b.Create(ctx, Spec{Name: "n1", Image: "i", MemoryMB: 2048, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	spec := created["spec"].(map[string]any)
	if spec["runtime"].(map[string]any)["ttl"] != "60m" || spec["network"] == nil {
		t.Fatalf("%v", spec)
	}
	r, err := b.Exec(ctx, "n1", Cmd{Shell: "it's", Timeout: time.Minute})
	if err != nil || r.Exit != 4 || r.Out != "o" || polls != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	l, err := b.List(ctx)
	if err != nil || len(l) != 2 {
		t.Fatalf("%v %v", l, err)
	}
	if err := b.Destroy(ctx, "n1", ""); err != nil {
		t.Fatal(err)
	}
	if quote("it's") != `'it'\''s'` {
		t.Fatal(quote("it's"))
	}
}

func TestTail(t *testing.T) {
	var x Tail
	for range 10 {
		_, _ = x.Write([]byte(strings.Repeat("a", 20<<10)))
	}
	_, _ = x.Write([]byte("end"))
	if s := x.String(); len(s) > TailBytes || !strings.HasSuffix(s, "end") {
		t.Fatal(len(s))
	}
}
