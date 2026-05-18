package storage

import (
	"testing"
)

func TestContentHash(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "known input",
			data: []byte("hello world"),
			want: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", // DevSkim: ignore DS173237 — SHA-256 test vector, not a credential
		},
		{
			name: "empty input",
			data: []byte{},
			want: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // DevSkim: ignore DS173237 — SHA-256 test vector, not a credential
		},
		{
			name: "binary data",
			data: []byte{0x00, 0x01, 0x02, 0xff},
			want: "3d1f57c984978ef98a18378c8166c1cb8ede02c03eeb6aee7e2f121dfeee3e56", // DevSkim: ignore DS173237 — SHA-256 test vector, not a credential
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContentHash(tt.data)
			if got != tt.want {
				t.Errorf("ContentHash() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContentKey(t *testing.T) {
	data := []byte("hello world")
	hash := ContentHash(data)
	want := "attestation/" + hash + ".json"

	got := ContentKey(data)
	if got != want {
		t.Errorf("ContentKey() = %q, want %q", got, want)
	}
}

func TestContentHash_Deterministic(t *testing.T) {
	data := []byte("determinism test")
	first := ContentHash(data)
	second := ContentHash(data)
	if first != second {
		t.Errorf("ContentHash not deterministic: %q != %q", first, second)
	}
}
