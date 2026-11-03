package config

import (
	"fmt"
	"strconv"
	"strings"
)

// A config path names a setting by its keys joined with dots, such as
// llm.tenant_overrides.acme-corp.timeout. A key that a dot-separated path
// cannot hold unambiguously (one containing '.', '[', ']' or '"', or an
// empty key) is written as a Go-quoted string in brackets instead:
// llm.tenant_overrides["a.b"].timeout. FieldError paths are written this
// way, and SplitPath reads them back, so `crosscodex config set` and unset
// can address every key an error names.

// mapKeyPath appends key, a map key or a key read from a file, to the path
// parent, quoting it when it needs it.
func mapKeyPath(parent, key string) string {
	if key == "" || strings.ContainsAny(key, `.[]"`) {
		return parent + "[" + strconv.Quote(key) + "]"
	}
	return joinPath(parent, key)
}

// FormatPath joins keys into a config path, quoting each key that needs
// it; SplitPath(FormatPath(keys...)) returns keys.
func FormatPath(keys ...string) string {
	path := ""
	for i, k := range keys {
		if i == 0 && k != "" && !strings.ContainsAny(k, `.[]"`) {
			path = k
			continue
		}
		path = mapKeyPath(path, k)
	}
	return path
}

// trackerKey converts a config path to the form the source tracker records
// paths in: keys joined with dots, unquoted. A list index ends it, because
// the tracker records a list as one value.
func trackerKey(path string) string {
	var keys []string
	rest := path
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, `["`):
			key, after, err := splitQuoted(path, rest)
			if err != nil {
				return strings.Join(keys, ".")
			}
			keys, rest = append(keys, key), after
		case strings.HasPrefix(rest, "["):
			return strings.Join(keys, ".")
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
		default:
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			keys, rest = append(keys, rest[:end]), rest[end:]
		}
	}
	return strings.Join(keys, ".")
}

// SplitPath splits a config path into its keys. It returns an
// ErrInvalidPath error for an empty path, an empty key, or a bracket that is
// not a complete ["key"] segment.
func SplitPath(path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("config path is empty: name a setting such as logging.level: %w", ErrInvalidPath)
	}
	var keys []string
	rest := path
	for {
		if strings.HasPrefix(rest, "[") {
			key, after, err := splitQuoted(path, rest)
			if err != nil {
				return nil, err
			}
			keys = append(keys, key)
			rest = after
		} else {
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			if end == 0 {
				return nil, fmt.Errorf("config path %q has an empty key: separate keys with single dots: %w", path, ErrInvalidPath)
			}
			if strings.ContainsAny(rest[:end], `]"`) {
				return nil, fmt.Errorf("config path %q: write a key containing ] or \" as [\"key\"]: %w", path, ErrInvalidPath)
			}
			keys = append(keys, rest[:end])
			rest = rest[end:]
		}
		switch {
		case rest == "":
			return keys, nil
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			if rest == "" || strings.HasPrefix(rest, ".") {
				return nil, fmt.Errorf("config path %q has an empty key: separate keys with single dots: %w", path, ErrInvalidPath)
			}
		case strings.HasPrefix(rest, "["):
		default:
			return nil, fmt.Errorf("config path %q has text after key %q; follow a key with . or [: %w", path, keys[len(keys)-1], ErrInvalidPath)
		}
	}
}

// splitQuoted reads the ["key"] segment rest starts with.
func splitQuoted(path, rest string) (key, after string, err error) {
	bad := fmt.Errorf(`config path %q has a bracket that is not a ["key"] segment with a Go-quoted key: %w`, path, ErrInvalidPath)
	if len(rest) < 2 || rest[1] != '"' {
		return "", "", bad
	}
	quoted, err := strconv.QuotedPrefix(rest[1:])
	if err != nil {
		return "", "", bad
	}
	after = rest[1+len(quoted):]
	if !strings.HasPrefix(after, "]") {
		return "", "", bad
	}
	key, err = strconv.Unquote(quoted)
	if err != nil {
		return "", "", bad
	}
	return key, after[1:], nil
}
