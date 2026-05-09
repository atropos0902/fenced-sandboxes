package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fenced/internal/fakegh"
)

func TestTokenIsNarrow(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	f := &fakegh.Server{Root: t.TempDir()}
	s := httptest.NewServer(f.Handler())
	defer s.Close()
	a, err := New(s.URL, s.URL, "1", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Token(context.Background(), "demo/shop"); err != nil {
		t.Fatal(err)
	}
	perms := f.TokenReq["permissions"].(map[string]any)
	if _, ok := perms["workflows"]; ok || len(perms) != 3 {
		t.Fatalf("permissions: %v", perms)
	}
	if r := f.TokenReq["repositories"].([]any); len(r) != 1 || r[0] != "shop" {
		t.Fatalf("repositories: %v", r)
	}
	j, _ := a.jwt(time.Now())
	i := strings.LastIndexByte(j, '.')
	sig, _ := base64.RawURLEncoding.DecodeString(j[i+1:])
	h := sha256.Sum256([]byte(j[:i]))
	if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, h[:], sig); err != nil {
		t.Fatal(err)
	}
}

func TestEnsurePRTwice(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	f := &fakegh.Server{Root: t.TempDir()}
	s := httptest.NewServer(f.Handler())
	defer s.Close()
	a, _ := New(s.URL, s.URL, "1", p)
	for range 2 {
		n, err := a.EnsurePR(context.Background(), "demo/shop", "agent/x", "main", "t", "b")
		if err != nil || n != 1 {
			t.Fatal(n, err)
		}
	}
	if len(f.PRs) != 1 {
		t.Fatalf("%d pull requests", len(f.PRs))
	}
}
