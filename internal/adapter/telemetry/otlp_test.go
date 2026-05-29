package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestSetupDisabledWhenEndpointEmpty(t *testing.T) {
	// A no-op provider is installed so we can assert Setup does not replace it.
	otel.SetTracerProvider(noop.NewTracerProvider())
	before := otel.GetTracerProvider()

	shutdown, err := Setup(t.Context(), OTLPConfig{})
	if err != nil {
		t.Fatalf("Setup with empty endpoint: unexpected error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Setup returned a nil shutdown func")
	}
	if got := otel.GetTracerProvider(); got != before {
		t.Errorf("Setup installed a provider when disabled: %T", got)
	}
	if err := shutdown(t.Context()); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
	}
}

func TestSetupBadProtocol(t *testing.T) {
	shutdown, err := Setup(t.Context(), OTLPConfig{
		Endpoint: "localhost:4317",
		Protocol: "carrier-pigeon",
	})
	if err == nil {
		t.Fatal("Setup with bad protocol: expected error, got nil")
	}
	if shutdown == nil {
		t.Fatal("Setup returned a nil shutdown func on error")
	}
	// The error shutdown must still be safe to call.
	if err := shutdown(t.Context()); err != nil {
		t.Errorf("error-path shutdown returned error: %v", err)
	}
}

func TestSetupGRPCDoesNotDial(t *testing.T) {
	// A bogus endpoint with Insecure must construct without blocking, because
	// the OTLP/gRPC exporter dials lazily on first export. Run under a deadline:
	// if Setup blocked on a dial, the context would expire and the test fail.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	type result struct {
		shutdown func(context.Context) error
		err      error
	}
	done := make(chan result, 1)
	go func() {
		sd, err := Setup(ctx, OTLPConfig{
			Endpoint: "127.0.0.1:1", // nothing listening here
			Protocol: ProtocolGRPC,
			Insecure: true,
			Timeout:  100 * time.Millisecond,
		})
		done <- result{sd, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Setup against unreachable endpoint errored: %v", res.err)
		}
		if res.shutdown == nil {
			t.Fatal("Setup returned a nil shutdown func")
		}
		if _, ok := otel.GetTracerProvider().(*noop.TracerProvider); ok {
			t.Error("Setup did not install a real TracerProvider")
		}
		// Shutdown should return promptly and not panic. Any error (e.g. a
		// flush failing because nothing is listening) is acceptable here; we
		// only assert it does not hang or panic.
		sdCtx, sdCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer sdCancel()
		_ = res.shutdown(sdCtx)
	case <-time.After(2 * time.Second):
		t.Fatal("Setup blocked: gRPC exporter should construct without dialing")
	}
}

func TestSamplerNeverNil(t *testing.T) {
	for _, ratio := range []float64{-1, 0, 0.25, 1, 2} {
		if s := sampler(ratio); s == nil {
			t.Errorf("sampler(%v) returned nil", ratio)
		}
	}
}
