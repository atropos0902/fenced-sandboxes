package gate

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"fenced/internal/config"
	"fenced/internal/obs"
	"fenced/internal/store"
)

// Egress is a CONNECT-only forward proxy. It does not read TLS. What it decides
// is which host a tunnel may go to and how long the tunnel may last.
type Egress struct {
	Store   *store.Store
	Config  *config.Config
	Log     *slog.Logger
	Recheck time.Duration
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
}

func HostAllowed(allow []string, host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, a := range allow {
		a = strings.ToLower(a)
		if host == a || strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) && len(host) > len(a)-1 {
			return true
		}
	}
	return false
}

var blocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "2002::/16", "2001::/32"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Public reports whether an address is outside every internal range. Metadata
// services live at 169.254.169.254, which is link-local.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func proxyToken(r *http.Request) string {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(r.Header.Get("Proxy-Authorization"), "Basic "))
	if err != nil {
		return ""
	}
	_, tok, _ := strings.Cut(string(b), ":")
	return tok
}

func (e *Egress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := proxyToken(r)
	if tok == "" {
		w.Header().Set("Proxy-Authenticate", `Basic realm="sandbox"`)
		http.Error(w, "proxy token required", http.StatusProxyAuthRequired)
		return
	}
	gr, err := e.Store.Grant(r.Context(), tok)
	if err != nil || !gr.Can("egress") {
		obs.GateRejected.WithLabelValues("stale_token").Inc()
		http.Error(w, "token is not valid for a current attempt", http.StatusForbidden)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if r.Method != http.MethodConnect || err != nil {
		// Plain HTTP would let a dependency be swapped on the way in.
		e.deny(w, r, gr, r.URL.Hostname(), "only CONNECT to port 443 is allowed")
		return
	}
	repo := e.Config.Repo(gr.Repo)
	if port != "443" || repo == nil || !HostAllowed(repo.AllowHosts, host) {
		e.deny(w, r, gr, host, "host is not on the allow list")
		return
	}
	// Resolve once and dial that address. Checking a name and then dialing the
	// name lets the second lookup answer differently.
	addrs, err := e.Resolve(r.Context(), host)
	if err != nil || len(addrs) == 0 {
		http.Error(w, "cannot resolve "+host, http.StatusBadGateway)
		return
	}
	for _, a := range addrs {
		if !Public(a) {
			e.deny(w, r, gr, host, "host resolves to an internal address")
			return
		}
	}
	var up net.Conn
	for _, a := range addrs {
		if up, err = e.Dial(r.Context(), "tcp", net.JoinHostPort(a.String(), "443")); err == nil {
			break
		}
	}
	if err != nil {
		http.Error(w, "cannot reach "+host, http.StatusBadGateway)
		return
	}
	defer up.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer down.Close()
	_, _ = io.WriteString(down, "HTTP/1.1 200 Connection established\r\n\r\n")
	e.tunnel(tok, buf, down, up)
}

// tunnel also closes when the token dies. Without that, an attempt that lost
// its run keeps whatever connections it already had.
func (e *Egress) tunnel(tok string, buffered io.Reader, down, up net.Conn) {
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done); down.Close(); up.Close() }) }
	go func() { _, _ = io.Copy(up, buffered); stop() }()
	go func() { _, _ = io.Copy(down, up); stop() }()
	t := time.NewTicker(e.Recheck)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), e.Recheck)
			_, err := e.Store.Grant(ctx, tok)
			cancel()
			if errors.Is(err, store.ErrFenced) {
				obs.TunnelsCut.Inc()
				stop()
				return
			}
		}
	}
}

func (e *Egress) deny(w http.ResponseWriter, r *http.Request, gr *store.Grant, host, why string) {
	obs.EgressDenied.Inc()
	if len(host) > 253 {
		host = host[:253]
	}
	if err := e.Store.Event(r.Context(), gr.RunID, gr.Epoch, "egress_denied",
		map[string]any{"host": host, "why": why, "sandbox": gr.Sandbox, "phase": gr.Phase}); err != nil {
		e.Log.Error("record denied egress", "err", err)
	}
	http.Error(w, fmt.Sprintf("%s: %s", host, why), http.StatusForbidden)
}

func DefaultResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}
