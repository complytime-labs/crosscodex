package oscal

import "testing"

func TestCleanForEmbedding(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "strips OSCAL template markers",
			text: "This text has {{ insert: param, value }} embedded.",
			want: "This text has embedded.",
		},
		{
			name: "strips markdown table separators",
			text: "Header\n|------|------|\nData",
			want: "Header\nData",
		},
		{
			name: "strips VerDate metadata",
			text: "Text\nVerDate Sep 11 2014 12:30 Jul 01, 2024\nMore text",
			want: "Text\nMore text",
		},
		{
			name: "strips Jkt metadata",
			text: "Content\nJkt 123456 PO 00000\nNext line",
			want: "Content\nNext line",
		},
		{
			name: "strips PO metadata",
			text: "Line 1\nPO 12345 Fmt 8010\nLine 2",
			want: "Line 1\nLine 2",
		},
		{
			name: "strips Frm/Fmt/Sfmt metadata",
			text: "Text\nFrm 00001 Fmt 8010 Sfmt 8010\nMore",
			want: "Text\nMore",
		},
		{
			name: "strips Windows path metadata",
			text: "Content\nG:\\COMP\\SOME\\PATH\\FILE.XML\nNext",
			want: "Content\nNext",
		},
		{
			name: "collapses excessive newlines (3+ → 2)",
			text: "Line 1\n\n\n\nLine 2",
			want: "Line 1\n\nLine 2",
		},
		{
			name: "collapses excessive spaces (2+ → 1)",
			text: "Word1    Word2     Word3",
			want: "Word1 Word2 Word3",
		},
		{
			name: "trims leading and trailing whitespace",
			text: "  \n\n  Text content  \n\n  ",
			want: "Text content",
		},
		{
			name: "handles multiple OSCAL templates",
			text: "{{ insert: param, a }} and {{ insert: param, b }} text",
			want: "and text",
		},
		{
			name: "handles all PDF artifacts together",
			text: `Document start
|------|------|
VerDate Sep 11 2014 12:30 Jul 01, 2024
Jkt 123456 PO 00000
PO 12345 Fmt 8010
Frm 00001 Fmt 8010 Sfmt 8010
G:\COMP\PATH\FILE.XML
Document end`,
			want: "Document start\nDocument end",
		},
		{
			name: "collapses four newlines to two",
			text: "A\n\n\n\nB",
			want: "A\n\nB",
		},
		{
			name: "collapses tabs and spaces",
			text: "Word1\t\t\tWord2  \t  Word3",
			want: "Word1 Word2 Word3",
		},
		{
			name: "preserves single newlines",
			text: "Line 1\nLine 2\nLine 3",
			want: "Line 1\nLine 2\nLine 3",
		},
		{
			name: "preserves double newlines",
			text: "Para 1\n\nPara 2",
			want: "Para 1\n\nPara 2",
		},
		{
			name: "empty string",
			text: "",
			want: "",
		},
		{
			name: "whitespace only",
			text: "   \n\n\n   ",
			want: "",
		},
		{
			name: "complex real-world example",
			text: `Control Statement

{{ insert: param, ac-1_prm_1 }}

The organization must:

|------|------|------|
a. Develop policies
b. Review procedures


VerDate Sep 11 2014 12:30 Jul 01, 2024
Jkt 123456 PO 00000 Frm 00001 Fmt 8010 Sfmt 8010
G:\COMP\NIST\SP800-53.XML


Final text.`,
			want: "Control Statement\n\nThe organization must:\n\na. Develop policies\nb. Review procedures\n\nFinal text.",
		},
		{
			name: "strips all VerDate variations",
			text: "VerDate Mar 15 2010 09:45 Dec 31, 2023 Jkt 041481\nMore content",
			want: "More content",
		},
		{
			name: "strips nested templates (non-greedy match)",
			text: "{{ outer {{ inner }} }} text",
			want: "}} text",
		},
		{
			name: "handles mixed artifact types on same line",
			text: "Text\nJkt 123 PO 45678 Fmt 8010\nmore",
			want: "Text\nmore",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CleanForEmbedding(tt.text)
			if got != tt.want {
				t.Errorf("CleanForEmbedding() = %q, want %q", got, tt.want)
			}
		})
	}
}
