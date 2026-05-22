package vectordb

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// newTestStore creates a PgVectorStore with a zero-value sql.DB for unit tests
// that only exercise pre-database validation paths. Options are applied in order.
func newTestStore(t *testing.T, opts ...Option) *PgVectorStore {
	t.Helper()
	store, err := NewPgVectorStore(&sql.DB{}, opts...)
	if err != nil {
		t.Fatalf("NewPgVectorStore() error = %v, want nil", err)
	}
	return store
}

// requireNoTenantErr asserts that err wraps tenant.ErrNoTenant.
func requireNoTenantErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Expected error for missing tenant context, but got nil")
	}
	if !errors.Is(err, tenant.ErrNoTenant) {
		t.Fatalf("Expected errors.Is(err, tenant.ErrNoTenant), got: %v", err)
	}
}

// requireTenantMismatchErr asserts that err wraps tenant.ErrTenantMismatch.
func requireTenantMismatchErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Expected error for tenant mismatch, but got nil")
	}
	if !errors.Is(err, tenant.ErrTenantMismatch) {
		t.Fatalf("Expected errors.Is(err, tenant.ErrTenantMismatch), got: %v", err)
	}
}

// mismatchCtx returns a context with "context-tenant" set, intended for use
// with a param tenant of "param-tenant" to trigger a mismatch error.
func mismatchCtx() context.Context {
	return tenant.WithTenant(context.Background(), "context-tenant")
}

// testFindSimilarQuery returns a FindSimilarQuery with standard test values.
// Override fields after construction when non-default values are needed.
func testFindSimilarQuery() FindSimilarQuery {
	return FindSimilarQuery{
		CatalogID: "test-catalog",
		Model:     "test-model",
		Vector:    []float32{0.1, 0.2, 0.3},
		Limit:     5,
	}
}

func TestNewPgVectorStore(t *testing.T) {
	store := newTestStore(t)

	// Verify store implements both interfaces
	var _ Index = store
	var _ VectorDB = store
}

func TestPgVectorStore_StoreEmbedding(t *testing.T) {
	store := newTestStore(t)

	embedding := Embedding{
		CatalogID: "test-catalog",
		ControlID: "test-control",
		Model:     "test-model",
		Vector:    []float32{0.1, 0.2, 0.3},
		Metadata: map[string]interface{}{
			"test": "value",
		},
	}

	t.Run("missing_tenant_context", func(t *testing.T) {
		requireNoTenantErr(t, store.StoreEmbedding(context.Background(), "test-tenant", embedding))
	})

	t.Run("tenant_mismatch", func(t *testing.T) {
		requireTenantMismatchErr(t, store.StoreEmbedding(mismatchCtx(), "param-tenant", embedding))
	})

	// Note: valid_tenant_context test omitted for scaffolding phase
	// The security-critical validation (missing context, tenant mismatch) is thoroughly tested above
	// Database operations will be tested in integration tests with real database connections
}

func TestPgVectorStore_FindSimilar(t *testing.T) {
	store := newTestStore(t)

	t.Run("missing_tenant_context", func(t *testing.T) {
		_, err := store.FindSimilar(context.Background(), "test-tenant", testFindSimilarQuery())
		requireNoTenantErr(t, err)
	})

	t.Run("tenant_mismatch", func(t *testing.T) {
		_, err := store.FindSimilar(mismatchCtx(), "param-tenant", testFindSimilarQuery())
		requireTenantMismatchErr(t, err)
	})

	t.Run("invalid_limit", func(t *testing.T) {
		ctx := tenant.WithTenant(context.Background(), "test-tenant")
		q := testFindSimilarQuery()
		q.Limit = 0
		_, err := store.FindSimilar(ctx, "test-tenant", q)
		if err == nil {
			t.Fatal("Expected error for invalid limit, but got nil")
		}
		if !strings.Contains(err.Error(), "limit must be positive") {
			t.Fatalf("Expected limit error, got: %v", err)
		}
	})

	t.Run("negative_limit", func(t *testing.T) {
		ctx := tenant.WithTenant(context.Background(), "test-tenant")
		q := testFindSimilarQuery()
		q.Limit = -1
		_, err := store.FindSimilar(ctx, "test-tenant", q)
		if err == nil {
			t.Fatal("Expected error for negative limit, but got nil")
		}
		if !strings.Contains(err.Error(), "limit must be positive") {
			t.Fatalf("Expected limit error, got: %v", err)
		}
	})

	t.Run("empty_vector", func(t *testing.T) {
		ctx := tenant.WithTenant(context.Background(), "test-tenant")
		q := testFindSimilarQuery()
		q.Vector = []float32{}
		_, err := store.FindSimilar(ctx, "test-tenant", q)
		if err == nil {
			t.Fatal("Expected error for empty vector, but got nil")
		}
		if !strings.Contains(err.Error(), "query vector cannot be empty") {
			t.Fatalf("Expected empty vector error, got: %v", err)
		}
	})

	t.Run("nil_vector", func(t *testing.T) {
		ctx := tenant.WithTenant(context.Background(), "test-tenant")
		q := testFindSimilarQuery()
		q.Vector = nil
		_, err := store.FindSimilar(ctx, "test-tenant", q)
		if err == nil {
			t.Fatal("Expected error for nil vector, but got nil")
		}
		if !strings.Contains(err.Error(), "query vector cannot be empty") {
			t.Fatalf("Expected empty vector error, got: %v", err)
		}
	})
}

func TestPgVectorStore_StoreBatch(t *testing.T) {
	store := newTestStore(t)

	embeddings := []Embedding{
		{CatalogID: "cat1", ControlID: "ctrl1", Model: "model1", Vector: []float32{0.1}},
		{CatalogID: "cat1", ControlID: "ctrl2", Model: "model1", Vector: []float32{0.2}},
	}

	t.Run("missing_tenant_context", func(t *testing.T) {
		requireNoTenantErr(t, store.StoreBatch(context.Background(), "test-tenant", embeddings))
	})

	t.Run("tenant_mismatch", func(t *testing.T) {
		requireTenantMismatchErr(t, store.StoreBatch(mismatchCtx(), "param-tenant", embeddings))
	})

	t.Run("empty_batch", func(t *testing.T) {
		ctx := tenant.WithTenant(context.Background(), "test-tenant")
		err := store.StoreBatch(ctx, "test-tenant", []Embedding{})
		if err != nil {
			t.Fatalf("Expected nil error for empty batch, got: %v", err)
		}
	})
}

func TestPgVectorStore_IndexMethods(t *testing.T) {
	store := newTestStore(t)

	t.Run("Insert", func(t *testing.T) {
		t.Run("missing_tenant_context", func(t *testing.T) {
			requireNoTenantErr(t, store.Insert(context.Background(), "ctrl-1", []float32{0.1, 0.2}, nil))
		})

		// Note: delegation to StoreEmbedding (default metadata, custom metadata)
		// requires a real database connection. Tested in integration tests.
	})

	t.Run("Search", func(t *testing.T) {
		t.Run("missing_tenant_context", func(t *testing.T) {
			_, err := store.Search(context.Background(), []float32{0.1, 0.2}, 5)
			requireNoTenantErr(t, err)
		})
	})

	t.Run("Delete", func(t *testing.T) {
		t.Run("missing_tenant_context", func(t *testing.T) {
			requireNoTenantErr(t, store.Delete(context.Background(), "ctrl-1"))
		})
	})

	t.Run("Get", func(t *testing.T) {
		t.Run("missing_tenant_context", func(t *testing.T) {
			_, err := store.Get(context.Background(), "ctrl-1")
			requireNoTenantErr(t, err)
		})
	})

	t.Run("Count", func(t *testing.T) {
		t.Run("missing_tenant_context", func(t *testing.T) {
			_, err := store.Count(context.Background())
			requireNoTenantErr(t, err)
		})
	})
}

func TestParseVectorString(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []float32
		wantErr bool
	}{
		{
			name:  "standard_vector",
			input: "[1.0,2.0,3.0]",
			want:  []float32{1.0, 2.0, 3.0},
		},
		{
			name:  "single_element",
			input: "[0.5]",
			want:  []float32{0.5},
		},
		{
			name:  "negative_values",
			input: "[-1.5,2.5,-3.5]",
			want:  []float32{-1.5, 2.5, -3.5},
		},
		{
			name:  "integer_values",
			input: "[1,2,3]",
			want:  []float32{1, 2, 3},
		},
		{
			name:  "scientific_notation",
			input: "[1e-5,2.5e3]",
			want:  []float32{1e-5, 2.5e3},
		},
		{
			name:    "empty_brackets",
			input:   "[]",
			want:    nil,
			wantErr: false,
		},
		{
			name:    "invalid_format_no_brackets",
			input:   "1.0,2.0",
			wantErr: true,
		},
		{
			name:    "invalid_number",
			input:   "[1.0,abc,3.0]",
			wantErr: true,
		},
		{
			name:    "empty_string",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVectorString(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseVectorString(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Fatalf("parseVectorString(%q) = %v (len %d), want %v (len %d)",
						tt.input, got, len(got), tt.want, len(tt.want))
				}
				for i := range tt.want {
					if got[i] != tt.want[i] {
						t.Errorf("parseVectorString(%q)[%d] = %v, want %v", tt.input, i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestNewPgVectorStoreWithTelemetry(t *testing.T) {
	t.Run("nil_telemetry_fields_without_options", func(t *testing.T) {
		store := newTestStore(t)

		// Without WithTelemetry, all telemetry fields should be nil/zero
		if store.tracer != nil {
			t.Error("expected nil tracer without telemetry provider")
		}
		if store.meter != nil {
			t.Error("expected nil meter without telemetry provider")
		}
		if store.searchCounter != nil {
			t.Error("expected nil searchCounter without telemetry provider")
		}
		if store.searchLatency != nil {
			t.Error("expected nil searchLatency without telemetry provider")
		}
		if store.storeCounter != nil {
			t.Error("expected nil storeCounter without telemetry provider")
		}
		if store.storeLatency != nil {
			t.Error("expected nil storeLatency without telemetry provider")
		}
	})

	t.Run("with_telemetry_option", func(t *testing.T) {
		tp := tracenoop.NewTracerProvider()
		tracer := tp.Tracer("vectordb-test")
		mp := metricnoop.NewMeterProvider()
		meter := mp.Meter("vectordb-test")

		store := newTestStore(t, WithTelemetry(tracer, meter))

		if store.tracer == nil {
			t.Error("expected non-nil tracer with telemetry provider")
		}
		if store.meter == nil {
			t.Error("expected non-nil meter with telemetry provider")
		}
		if store.searchCounter == nil {
			t.Error("expected non-nil searchCounter with telemetry provider")
		}
		if store.searchLatency == nil {
			t.Error("expected non-nil searchLatency with telemetry provider")
		}
		if store.storeCounter == nil {
			t.Error("expected non-nil storeCounter with telemetry provider")
		}
		if store.storeLatency == nil {
			t.Error("expected non-nil storeLatency with telemetry provider")
		}
	})

	t.Run("nil_db_with_telemetry_still_fails", func(t *testing.T) {
		tp := tracenoop.NewTracerProvider()
		tracer := tp.Tracer("vectordb-test")
		mp := metricnoop.NewMeterProvider()
		meter := mp.Meter("vectordb-test")

		_, err := NewPgVectorStore(nil, WithTelemetry(tracer, meter))
		if err == nil {
			t.Fatal("expected error for nil db, got nil")
		}
		if !strings.Contains(err.Error(), "database connection is required") {
			t.Fatalf("expected 'database connection is required' error, got: %v", err)
		}
	})
}

func TestPgVectorStore_DeleteByModel(t *testing.T) {
	store := newTestStore(t)

	t.Run("missing_tenant_context", func(t *testing.T) {
		requireNoTenantErr(t, store.DeleteByModel(context.Background(), "test-tenant", "test-catalog", "test-model"))
	})

	t.Run("tenant_mismatch", func(t *testing.T) {
		requireTenantMismatchErr(t, store.DeleteByModel(mismatchCtx(), "param-tenant", "test-catalog", "test-model"))
	})
}
