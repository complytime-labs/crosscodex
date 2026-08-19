package analyzer_test

import (
	"context"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

func TestCatalogIDContext_RoundTrip(t *testing.T) {
	ctx := analyzer.WithCatalogID(context.Background(), "cat-1")
	got, ok := analyzer.CatalogIDFromContext(ctx)
	if !ok || got != "cat-1" {
		t.Fatalf("CatalogIDFromContext() = %q, %v; want \"cat-1\", true", got, ok)
	}
}

func TestCatalogIDContext_Absent(t *testing.T) {
	_, ok := analyzer.CatalogIDFromContext(context.Background())
	if ok {
		t.Fatal("CatalogIDFromContext() ok = true on empty context; want false")
	}
}
