package vectordb

import (
	"testing"
)

func TestErrorTypes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "incompatible model error",
			err:  ErrIncompatibleModel,
			want: "query model does not match stored embeddings",
		},
		{
			name: "model not found error",
			err:  ErrModelNotFound,
			want: "no embeddings found for specified model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("error message = %q, want %q", got, tt.want)
			}
		})
	}
}
