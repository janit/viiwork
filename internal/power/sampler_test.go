package power

import (
	"context"
	"testing"
)

func newTestSampler(available bool, watts float64) *Sampler {
	s := &Sampler{}
	s.available.Store(available)
	s.lastWatts = watts
	return s
}

func TestSamplerWithMockCommand(t *testing.T) {
	s := newTestSampler(true, 0)
	s.cmdFactory = func(ctx context.Context) ([]byte, error) {
		return []byte("PS1 Input Power    | 64h | ok  | 10.1 | 280 Watts\n"), nil
	}
	s.Sample(context.Background())

	if s.Watts() != 280.0 {
		t.Errorf("expected 280.0, got %f", s.Watts())
	}
}

func TestSamplerUnavailable(t *testing.T) {
	s := newTestSampler(false, 0)
	s.Sample(context.Background())
	if s.Watts() != 0.0 {
		t.Errorf("expected 0.0 when unavailable, got %f", s.Watts())
	}
	if s.Available() {
		t.Error("expected Available() = false")
	}
}

func TestSamplerKeepsLastValueOnError(t *testing.T) {
	s := newTestSampler(true, 300.0)
	s.cmdFactory = func(ctx context.Context) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	s.Sample(context.Background())
	if s.Watts() != 300.0 {
		t.Errorf("expected 300.0 (last value), got %f", s.Watts())
	}
}

func TestSamplerThreadSafe(t *testing.T) {
	s := newTestSampler(true, 0)
	s.cmdFactory = func(ctx context.Context) ([]byte, error) {
		return []byte("PS1 Input Power    | 64h | ok  | 10.1 | 100 Watts\n"), nil
	}

	done := make(chan struct{})
	go func() {
		for range 100 {
			s.Sample(context.Background())
		}
		close(done)
	}()
	for range 100 {
		_ = s.Watts()
		_ = s.Available()
	}
	<-done
}
