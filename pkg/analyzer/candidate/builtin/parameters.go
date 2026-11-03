package builtin

import (
	"fmt"
	"math"
	"strings"
)

// Parameters arrive as interface{} values whose Go type depends on the
// decoder that produced them: YAML decodes 75 as int and 72.5 as float64,
// JSON decodes every number as float64. Reading them with a single type
// assertion would silently drop a valid value of the other shape, so these
// helpers accept every numeric shape and return an error, never the
// default, for a value they cannot use.

// maxExactFloatInt is the largest integer float64 represents exactly.
//
// These readers' accepted shapes are mirrored by config validation in
// pkg/config/validate.go (isPositiveInteger, isSimilarityThreshold,
// isNonEmptyStringList); change both together.
const maxExactFloatInt = 1 << 53

// intParam returns params[key] as an int, or def when the key is absent.
// An integral float64 is accepted because JSON has no integer type.
func intParam(generator string, params map[string]interface{}, key string, def int) (int, error) {
	v, ok := params[key]
	if !ok {
		return def, nil
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		if n >= math.MinInt && n <= math.MaxInt {
			return int(n), nil
		}
	case float64:
		if n == math.Trunc(n) && math.Abs(n) <= maxExactFloatInt {
			return int(n), nil
		}
	}
	return 0, fmt.Errorf("%s: parameter %s %s must be an integer", generator, key, describeParam(v))
}

// floatParam returns params[key] as a float64, or def when the key is absent.
func floatParam(generator string, params map[string]interface{}, key string, def float64) (float64, error) {
	v, ok := params[key]
	if !ok {
		return def, nil
	}
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	}
	return 0, fmt.Errorf("%s: parameter %s %s must be a number", generator, key, describeParam(v))
}

// stringListParam returns params[key] as a non-empty list of non-blank
// strings, or def when the key is absent. YAML and JSON decode a list as
// []interface{}; a Go caller may pass []string. A blank entry is rejected
// because the empty string is a substring of every text.
func stringListParam(generator string, params map[string]interface{}, key string, def []string) ([]string, error) {
	v, ok := params[key]
	if !ok {
		return def, nil
	}
	invalid := fmt.Errorf("%s: parameter %s %s must be a non-empty list of non-empty strings", generator, key, describeParam(v))
	var items []string
	switch l := v.(type) {
	case []string:
		items = l
	case []interface{}:
		items = make([]string, len(l))
		for i, item := range l {
			s, ok := item.(string)
			if !ok {
				return nil, invalid
			}
			items[i] = s
		}
	default:
		return nil, invalid
	}
	if len(items) == 0 {
		return nil, invalid
	}
	for _, s := range items {
		if strings.TrimSpace(s) == "" {
			return nil, invalid
		}
	}
	return items, nil
}

// describeParam renders a value with its Go type, quoting strings so "3"
// is distinguishable from 3.
func describeParam(v interface{}) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q (string)", s)
	}
	return fmt.Sprintf("%v (%T)", v, v)
}
