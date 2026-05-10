package gate

import (
	"encoding/base64"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestHostAllowed(t *testing.T) {
	allow := []string{"registry.npmjs.org", "*.pkg.example"}
	for h, want := range map[string]bool{
		"registry.npmjs.org": true, "REGISTRY.npmjs.org.": true, "a.pkg.example": true, "a.b.pkg.example": true,
		"pkg.example": false, ".pkg.example": false, "evilpkg.example": false, "registry.npmjs.org.evil": false, "": false,
	} {
		if got := HostAllowed(allow, h); got != want {
			t.Errorf("%q: %v", h, got)
		}
	}
}

func TestPublic(t *testing.T) {
	for a, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true, "10.1.2.3": false, "172.17.0.1": false, "192.168.0.1": false,
		"127.0.0.1": false, "169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false,
		"fd00::1": false, "::ffff:10.0.0.1": false, "64:ff9b::a9fe:a9fe": false,
	} {
		if got := Public(netip.MustParseAddr(a)); got != want {
			t.Errorf("%s: %v", a, got)
		}
	}
}

func TestReadCommands(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	line := func(s string) string {
		const x = "0123456789abcdef"
		n := len(s) + 4
		return "00" + string(x[n>>4]) + string(x[n&15]) + s
	}
	cmds, raw, err := ReadCommands(strings.NewReader(line(a+" "+b+" refs/heads/x\x00 report-status") + "0000PACK"))
	if err != nil || len(cmds) != 1 || cmds[0] != (Command{a, b, "refs/heads/x"}) || strings.Contains(string(raw), "PACK") {
		t.Fatalf("%v %v %q", cmds, err, raw)
	}
	for _, bad := range []string{"", "zzzz", "0003", "0005x", line("push-cert x") + "0000", line(a+" "+b) + "0000", "00ff" + a} {
		if _, _, err := ReadCommands(strings.NewReader(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPreviewLink(t *testing.T) {
	key, now := []byte(strings.Repeat("k", 32)), time.Now()
	tok := Sign(key, "run", "head", now.Add(time.Minute))
	if !valid(key, "run", "head", tok, now) {
		t.Fatal("good link refused")
	}
	for name, ok := range map[string]bool{
		"other run": valid(key, "run2", "head", tok, now), "other commit": valid(key, "run", "head2", tok, now),
		"expired": valid(key, "run", "head", tok, now.Add(2*time.Minute)), "empty": valid(key, "run", "head", "", now),
		"stretched": valid(key, "run", "head", "9999999999"+tok[10:], now),
	} {
		if ok {
			t.Errorf("%s accepted", name)
		}
	}
}
