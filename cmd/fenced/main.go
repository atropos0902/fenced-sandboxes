// fenced gate      what sandboxes talk to. No provider access.
// fenced control   API, workers and reconciler. No sandbox shares its network.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"fenced/internal/api"
	"fenced/internal/config"
	"fenced/internal/gate"
	"fenced/internal/github"
	"fenced/internal/obs"
	"fenced/internal/provider"
	"fenced/internal/store"
	"fenced/internal/worker"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func seconds(k string, def int) time.Duration {
	n, err := strconv.Atoi(env(k, strconv.Itoa(def)))
	if err != nil {
		n = def
	}
	return time.Duration(n) * time.Second
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fenced gate|control")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, log.With("process", os.Args[1]), os.Args[1]); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, mode string) error {
	cfg, err := config.Load(env("CONFIG", "/etc/fenced/config.json"))
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	shutdown, err := obs.Tracing(ctx, "fenced-"+mode)
	if err != nil {
		return err
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	key, err := os.ReadFile(os.Getenv("GITHUB_APP_KEY_FILE"))
	if err != nil {
		return err
	}
	gh, err := github.New(env("GITHUB_API", "https://api.github.com"), env("GITHUB_GIT", "https://github.com"), os.Getenv("GITHUB_APP_ID"), key)
	if err != nil {
		return err
	}
	previewKey := []byte(os.Getenv("PREVIEW_KEY"))
	if len(previewKey) < 32 {
		return fmt.Errorf("PREVIEW_KEY needs at least 32 bytes")
	}
	switch mode {
	case "gate":
		g := &gate.Gate{Store: st, Config: cfg, Up: gh, Log: log, HC: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
		d := &net.Dialer{Timeout: 10 * time.Second}
		eg := &gate.Egress{Store: st, Config: cfg, Log: log, Recheck: seconds("TUNNEL_RECHECK_SECONDS", 5), Resolve: gate.DefaultResolve, Dial: d.DialContext}
		pv := &gate.Preview{Store: st, Domain: env("PREVIEW_DOMAIN", "preview.localhost"), Key: previewKey}
		return serve(ctx, log, map[string]http.Handler{":8080": g.Handler(), ":8081": eg, ":8091": pv, "127.0.0.1:9100": promhttp.Handler()})
	case "control":
		p, err := newProvider()
		if err != nil {
			return err
		}
		host, _ := os.Hostname()
		for i := range int(seconds("WORKERS", 2) / time.Second) {
			w := &worker.Worker{ID: fmt.Sprintf("%s-%d", host, i), Store: st, Config: cfg, Provider: p, GitHub: gh, Log: log,
				GateURL: env("GATE_URL", "http://gate:8080"), ProxyHost: env("PROXY_HOST", "gate:8081"),
				Lease: seconds("LEASE_SECONDS", 30), PreviewTTL: seconds("PREVIEW_TTL_SECONDS", 3600),
				LeaveOld: os.Getenv("FAULT_LEAVE_OLD_SANDBOX") == "1"}
			if w.Lease <= gate.PushTimeout {
				return fmt.Errorf("lease %s must be longer than the push timeout %s", w.Lease, gate.PushTimeout)
			}
			go w.Loop(ctx)
		}
		rc := &worker.Reconciler{Store: st, Provider: p, Log: log, Grace: seconds("GRACE_SECONDS", 60), Keep: 2}
		go rc.Loop(ctx, seconds("RECONCILE_SECONDS", 15))
		a := &api.API{Store: st, Config: cfg, Token: os.Getenv("API_TOKEN"), PreviewKey: previewKey,
			PreviewURL: env("PREVIEW_URL", "http://%s.preview.localhost:8091")}
		return serve(ctx, log, map[string]http.Handler{":8090": a.Handler()})
	}
	return fmt.Errorf("unknown mode %q", mode)
}

func newProvider() (provider.Provider, error) {
	owner := env("OWNER", "local")
	switch p := env("PROVIDER", "docker"); p {
	case "docker":
		return provider.NewDocker(env("DOCKER_SOCK", "/var/run/docker.sock"), owner, env("GATE_CONTAINER", "fenced-gate"), os.Getenv("DOCKER_RUNTIME")), nil
	case "e2b":
		return provider.NewE2B(os.Getenv("E2B_API_KEY"), owner, strings.Split(os.Getenv("GATE_CIDRS"), ",")), nil
	case "blaxel":
		b := provider.NewBlaxel(os.Getenv("BL_API_KEY"), os.Getenv("BL_WORKSPACE"), owner, os.Getenv("GATE_HOST"))
		b.Version = os.Getenv("BL_API_VERSION")
		return b, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", p)
	}
}

func serve(ctx context.Context, log *slog.Logger, hs map[string]http.Handler) error {
	errs := make(chan error, len(hs))
	var srvs []*http.Server
	for addr, h := range hs {
		s := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		srvs = append(srvs, s)
		go func() { errs <- s.ListenAndServe() }()
	}
	log.Info("listening")
	var err error
	select {
	case <-ctx.Done():
		time.Sleep(time.Second) // let workers hand their runs back
	case err = <-errs:
	}
	sd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range srvs {
		_ = s.Shutdown(sd)
	}
	return err
}
