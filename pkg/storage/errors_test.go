package storage

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinelErrors_NonNil(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrNotFound", ErrNotFound},
		{"ErrInvalidKey", ErrInvalidKey},
		{"ErrProviderClosed", ErrProviderClosed},
	}

	for _, s := range sentinels {
		if s.err == nil {
			t.Errorf("%s is nil", s.name)
		}
	}
}

func TestSentinelErrors_Distinct(t *testing.T) {
	sentinels := []error{ErrNotFound, ErrInvalidKey, ErrProviderClosed}

	for i := 0; i < len(sentinels); i++ {
		for j := i + 1; j < len(sentinels); j++ {
			if errors.Is(sentinels[i], sentinels[j]) {
				t.Errorf("sentinel %d and %d are not distinct", i, j)
			}
		}
	}
}

func TestSentinelErrors_Matchable(t *testing.T) {
	wrapped := fmt.Errorf("get failed: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Errorf("wrapped ErrNotFound not matchable via errors.Is")
	}
}
