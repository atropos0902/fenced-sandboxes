package obs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpansAreExported(t *testing.T) {
	got := make(chan string, 4)
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got <- r.URL.Path }))
	defer s.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", s.URL)
	stop, err := Tracing(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := Tracer.Start(context.Background(), "attempt")
	span.End()
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := <-got; !strings.HasSuffix(p, "/v1/traces") {
		t.Fatal(p)
	}
}
