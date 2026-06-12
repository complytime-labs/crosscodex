package oscal

import "testing"

func TestCleanProse(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		params map[string]string
		want   string
	}{
		{
			name: "substitutes parameter references with labels",
			text: "The system must use {{ insert: param, ac-1_prm_1 }} to authenticate.",
			params: map[string]string{
				"ac-1_prm_1": "multi-factor authentication",
			},
			want: "The system must use multi-factor authentication to authenticate.",
		},
		{
			name: "uses [param_id] fallback when param not found",
			text: "Configure {{ insert: param, missing_param }} before deployment.",
			params: map[string]string{
				"ac-1_prm_1": "value",
			},
			want: "Configure [missing_param] before deployment.",
		},
		{
			name:   "replaces unrecognized template patterns with [parameter]",
			text:   "This uses {{ unknown: template }} for configuration.",
			params: map[string]string{},
			want:   "This uses [parameter] for configuration.",
		},
		{
			name: "handles multiple substitutions in one string",
			text: "Use {{ insert: param, auth_method }} for {{ insert: param, user_type }} users.",
			params: map[string]string{
				"auth_method": "PKI",
				"user_type":   "privileged",
			},
			want: "Use PKI for privileged users.",
		},
		{
			name: "handles mixed found and missing parameters",
			text: "{{ insert: param, found }} and {{ insert: param, missing }} configured.",
			params: map[string]string{
				"found": "value1",
			},
			want: "value1 and [missing] configured.",
		},
		{
			name:   "returns text unchanged when no templates present",
			text:   "This is plain text without any templates.",
			params: map[string]string{},
			want:   "This is plain text without any templates.",
		},
		{
			name: "handles param IDs with hyphens and dots",
			text: "{{ insert: param, ac-1.2_prm_3 }} is supported.",
			params: map[string]string{
				"ac-1.2_prm_3": "configured value",
			},
			want: "configured value is supported.",
		},
		{
			name: "handles templates with varying whitespace",
			text: "{{insert:param,no_space}} and {{ insert: param, with_space }} work.",
			params: map[string]string{
				"no_space":   "value1",
				"with_space": "value2",
			},
			want: "value1 and value2 work.",
		},
		{
			name:   "empty params map with template",
			text:   "{{ insert: param, some_param }}",
			params: map[string]string{},
			want:   "[some_param]",
		},
		{
			name:   "empty text",
			text:   "",
			params: map[string]string{"key": "value"},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CleanProse(tt.text, tt.params)
			if got != tt.want {
				t.Errorf("CleanProse() = %q, want %q", got, tt.want)
			}
		})
	}
}
