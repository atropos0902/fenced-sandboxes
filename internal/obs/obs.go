// Package obs holds metrics and tracing setup.
package obs

import (
	"context"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

var (
	Phase = promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "fenced_phase_seconds", Help: "Time per phase.",
		Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1800}}, []string{"phase", "outcome"})
	Start = promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "fenced_ready_seconds", Help: "Create to ready for the agent.",
		Buckets: []float64{.25, .5, 1, 2, 3, 5, 8, 13, 21, 34, 60, 120, 300}}, []string{"start"})
	Failures     = promauto.NewCounterVec(prometheus.CounterOpts{Name: "fenced_failures_total", Help: "Failed attempts by cause."}, []string{"code", "owner"})
	Takeovers    = promauto.NewCounter(prometheus.CounterOpts{Name: "fenced_takeovers_total", Help: "Runs claimed at epoch 2 or later."})
	StalePush    = promauto.NewCounter(prometheus.CounterOpts{Name: "fenced_stale_push_total", Help: "Pushes refused at the lock because the attempt had been replaced."})
	GateRejected = promauto.NewCounterVec(prometheus.CounterOpts{Name: "fenced_gate_rejected_total", Help: "Requests the gate refused."}, []string{"why"})
	EgressDenied = promauto.NewCounter(prometheus.CounterOpts{Name: "fenced_egress_denied_total", Help: "Tunnels refused."})
	TunnelsCut   = promauto.NewCounter(prometheus.CounterOpts{Name: "fenced_tunnels_cut_total", Help: "Tunnels closed because the token died."})
	Open         = promauto.NewGauge(prometheus.GaugeOpts{Name: "fenced_sandboxes_open", Help: "Sandboxes the provider reports."})
	Leaked       = promauto.NewCounter(prometheus.CounterOpts{Name: "fenced_sandboxes_leaked_total", Help: "Unwanted sandboxes the reconciler removed."})
	Lost         = promauto.NewCounter(prometheus.CounterOpts{Name: "fenced_sandboxes_lost_total", Help: "Wanted sandboxes the provider no longer has."})
	Reconciled   = promauto.NewGauge(prometheus.GaugeOpts{Name: "fenced_reconciled_timestamp_seconds", Help: "Last complete reconcile."})
)

var Tracer = otel.Tracer("fenced")

// Tracing exports only when an OTLP endpoint is configured.
func Tracing(ctx context.Context, service string) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewSchemaless(semconv.ServiceName(service))))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
