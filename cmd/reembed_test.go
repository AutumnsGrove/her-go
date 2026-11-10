package cmd

import (
	"errors"
	"strings"
	"testing"
)

type embedFn func(string) ([]float32, error)

func (f embedFn) Embed(text string) ([]float32, error) { return f(text) }

// probeEmbedder is what stands between `her reembed` and an empty index: it must
// reject a failing provider and a mismatched dimension before anything is dropped.
func TestProbeEmbedder(t *testing.T) {
	good := embedFn(func(string) ([]float32, error) { return make([]float32, 1024), nil })
	if err := probeEmbedder(good, 1024); err != nil {
		t.Errorf("a healthy provider at the right width failed the probe: %v", err)
	}

	down := embedFn(func(string) ([]float32, error) { return nil, errors.New("503 service unavailable") })
	if err := probeEmbedder(down, 1024); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("a failing provider must fail the probe and keep its cause, got %v", err)
	}

	wrongWidth := embedFn(func(string) ([]float32, error) { return make([]float32, 768), nil })
	err := probeEmbedder(wrongWidth, 1024)
	if err == nil || !strings.Contains(err.Error(), "768") || !strings.Contains(err.Error(), "1024") {
		t.Errorf("a width mismatch must name both numbers, got %v", err)
	}
}
