package catalog

import (
	"strings"
	"testing"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// generateLongString creates a string of repeated characters for boundary testing
func generateLongString(length int) string {
	return strings.Repeat("A", length)
}

func TestExtractCatalogName(t *testing.T) {
	tests := []struct {
		name      string
		oscalJSON string
		want      string
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
		{
			name: "title with unicode characters",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": "NIST SP 800-53 Rev. 5 — Security & Privacy Controls 🔒"
					}
				}
			}`,
			want: "NIST SP 800-53 Rev. 5 — Security & Privacy Controls 🔒",
		},
		{
			name: "title with escaped quotes",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": "The \"Advanced\" Catalog"
					}
				}
			}`,
			want: `The "Advanced" Catalog`,
		},
		{
			name: "title with newlines and tabs",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": "Multi\nLine\tTitle"
					}
				}
			}`,
			want: "Multi\nLine\tTitle",
		},
		{
			name: "title with backslashes",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": "Path\\To\\Catalog"
					}
				}
			}`,
			want: `Path\To\Catalog`,
		},
		{
			name: "title is null",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": null
					}
				}
			}`,
			want: "",
		},
		{
			name: "title is number",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": 12345
					}
				}
			}`,
			want: "",
		},
		{
			name: "title is boolean",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": true
					}
				}
			}`,
			want: "",
		},
		{
			name: "title is array",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": ["Catalog", "Name"]
					}
				}
			}`,
			want: "",
		},
		{
			name: "title is object",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": {"en": "English Title"}
					}
				}
			}`,
			want: "",
		},
		{
			name: "very long title",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": "` + generateLongString(5000) + `"
					}
				}
			}`,
			want: generateLongString(5000),
		},
		{
			name: "empty string title",
			oscalJSON: `{
				"catalog": {
					"metadata": {
						"title": ""
					}
				}
			}`,
			want: "",
		},
		{
			name: "metadata is null",
			oscalJSON: `{
				"catalog": {
					"metadata": null
				}
			}`,
			want: "",
		},
		{
			name: "catalog is null",
			oscalJSON: `{
				"catalog": null
			}`,
			want: "",
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
		{
			name:   "oscal with leading whitespace",
			format: " oscal",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "oscal with trailing whitespace",
			format: "oscal ",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "oscal with surrounding whitespace",
			format: " oscal ",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "gemara with tabs",
			format: "\tgemara\t",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "oscal with newline",
			format: "oscal\n",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "partial oscal",
			format: "osc",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "partial gemara",
			format: "gem",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "oscal with suffix",
			format: "oscal-json",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "oscal with prefix",
			format: "json-oscal",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "typo osacl",
			format: "osacl",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "typo gemera",
			format: "gemera",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "numeric string",
			format: "123",
			want:   crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
		},
		{
			name:   "special characters",
			format: "!@#$%",
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

func TestIsOSCALJSON(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			name: "valid OSCAL JSON",
			data: `{"catalog": {"metadata": {"title": "Test"}}}`,
			want: true,
		},
		{
			name: "OSCAL with empty catalog",
			data: `{"catalog": {}}`,
			want: true,
		},
		{
			name: "catalog is null",
			data: `{"catalog": null}`,
			want: true,
		},
		{
			name: "not OSCAL - missing catalog key",
			data: `{"metadata": {"title": "Test"}}`,
			want: false,
		},
		{
			name: "invalid JSON",
			data: `not valid json`,
			want: false,
		},
		{
			name: "empty string",
			data: ``,
			want: false,
		},
		{
			name: "JSON array",
			data: `[{"catalog": {}}]`,
			want: false,
		},
		{
			name: "JSON string",
			data: `"catalog"`,
			want: false,
		},
		{
			name: "JSON number",
			data: `123`,
			want: false,
		},
		{
			name: "JSON boolean",
			data: `true`,
			want: false,
		},
		{
			name: "JSON null",
			data: `null`,
			want: false,
		},
		{
			name: "catalog with wrong case",
			data: `{"Catalog": {}}`,
			want: false,
		},
		{
			name: "catalog with extra whitespace",
			data: `{  "catalog"  :  {}  }`,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isOSCALJSON([]byte(tt.data))
			if got != tt.want {
				t.Errorf("isOSCALJSON() = %v, want %v", got, tt.want)
			}
		})
	}
}
