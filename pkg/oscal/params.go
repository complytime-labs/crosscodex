package oscal

import "regexp"

var (
	templatePattern = regexp.MustCompile(`\{\{.*?\}\}`)
	paramRefPattern = regexp.MustCompile(`insert:\s*param,\s*([\w\-\.]+)`)
)

// CleanProse substitutes OSCAL parameter references in prose text.
//
// Pattern: {{ insert: param, <id> }} → resolved label/value.
//
// When a parameter ID is found in the params map, it is substituted with the
// corresponding value. When a parameter ID is not found, it is replaced with
// [param_id]. Unrecognized template patterns are replaced with [parameter].
func CleanProse(text string, params map[string]string) string {
	return templatePattern.ReplaceAllStringFunc(text, func(match string) string {
		if m := paramRefPattern.FindStringSubmatch(match); m != nil {
			paramID := m[1]
			if val, ok := params[paramID]; ok {
				return val
			}
			return "[" + paramID + "]"
		}
		return "[parameter]"
	})
}
