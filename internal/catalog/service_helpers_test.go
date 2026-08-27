package catalog

import (
	"testing"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

func TestExtractCatalogName(t *testing.T) {
	tests := []struct {
		name     string
		oscalJSON string
		want     string
	}{
		{
			name: "valid OSCAL with metadata title",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": "CrossCodex E2E Minimal Catalog",
						"version": "1.0"
					}
				}
			}`,
			want: "CrossCodex E2E Minimal Catalog",
		},
		{
			name: "OSCAL without metadata",
			oscalJSON: `{
				"catalog": {
					"groups": []
				}
			}`,
			want: "",
		},
		{
			name: "OSCAL without title",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"version": "1.0"
					}
				}
			}`,
			want: "",
		},
		{
			name: "not OSCAL",
			oscalJSON: `{
				"some_other_root": {}
			}`,
			want: "",
		},
		{
			name:      "invalid JSON",
			oscalJSON: `not json`,
			want:      "",
		},
		{
			name:      "empty",
			oscalJSON: ``,
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCatalogName([]byte(tt.oscalJSON))
			if got != tt.want {
				t.Errorf("extractCatalogName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatStringToEnum(t *testing.T) {
	tests := []struct {
		name   string
		format string
		want   crosscodexv1.CatalogFormat
	}{
		{
			name:   "oscal lowercase",
			format: "oscal",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL,
		},
		{
			name:   "oscal uppercase",
			format: "OSCAL",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL,
		},
		{
			name:   "oscal mixed case",
			format: "OsCaL",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL,
		},
		{
			name:   "gemara lowercase",
			format: "gemara",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA,
		},
		{
			name:   "gemara uppercase",
			format: "GEMARA",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA,
		},
		{
			name:   "unknown format",
			format: "unknown",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "empty string",
			format: "",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatStringToEnum(tt.format)
			if got != tt.want {
				t.Errorf("formatStringToEnum(%q) = %v, want %v", tt.format, got, tt.want)
			}
		})
	}
}

func TestCatalogRecordToProto_FormatField(t *testing.T) {
	tests := []struct {
		name         string
		formatString string
		wantEnum     crosscodexv1.CatalogFormat
	}{
		{
			name:         "OSCAL format",
			formatString: "oscal",
			wantEnum:     crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL,
		},
		{
			name:         "Gemara format",
			formatString: "gemara",
			wantEnum:     crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA,
		},
		{
			name:         "unspecified format",
			formatString: "",
			wantEnum:     crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &CatalogRecord{
				CatalogID: "test-catalog-id",
				TenantID:  "test-tenant",
				Name:      "Test Catalog",
				Format:    tt.formatString,
			}

			proto := catalogRecordToProto(rec)

			if proto == nil {
				t.Fatal("catalogRecordToProto returned nil")
			}

			if proto.GetFormat() != tt.wantEnum {
				t.Errorf("catalogRecordToProto().Format = %v, want %v", proto.GetFormat(), tt.wantEnum)
			}

			if proto.GetName() != "Test Catalog" {
				t.Errorf("catalogRecordToProto().Name = %q, want %q", proto.GetName(), "Test Catalog")
			}
		})
	}
}

func TestCatalogRecordToProto_NilRecord(t *testing.T) {
	proto := catalogRecordToProto(nil)
	if proto != nil {
		t.Errorf("catalogRecordToProto(nil) = %v, want nil", proto)
	}
}
