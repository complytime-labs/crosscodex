package prompt

import (
	"fmt"
	"regexp"
	"strings"
)

// placeholderRe matches ${name} where name is [a-zA-Z_][a-zA-Z0-9_]*.
var placeholderRe = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// SubstitutePlaceholders replaces all ${name} patterns in template with
// values from vars. Returns ErrMissingPlaceholder if any placeholder has
// no corresponding entry in vars. Extra vars are silently ignored.
// No recursive expansion -- if a var value contains ${x}, it stays literal.
func SubstitutePlaceholders(template string, vars map[string]string) (string, error) {
	var missing []string

	// First pass: find all placeholders and check for missing vars.
	matches := placeholderRe.FindAllStringSubmatch(template, -1)
	for _, match := range matches {
		name := match[1]
		if _, ok := vars[name]; !ok {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return "", fmt.Errorf("template references undefined variable(s) %s: %w",
			strings.Join(missing, ", "), ErrMissingPlaceholder)
	}

	// Second pass: replace all placeholders.
	result := placeholderRe.ReplaceAllStringFunc(template, func(match string) string {
		name := placeholderRe.FindStringSubmatch(match)[1]
		return vars[name]
	})

	return result, nil
}

// assembleMessages builds the []Message sequence from rendered templates
// and resolved few-shot examples. Order: system -> few-shot pairs -> user.
// Empty system or user templates are omitted.
func assembleMessages(system, user string, fewShot []FewShotExample) []Message {
	var msgs []Message

	if system != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: system})
	}

	for _, ex := range fewShot {
		if ex.Input != "" {
			msgs = append(msgs, Message{Role: RoleUser, Content: ex.Input})
		}
		if ex.Output != "" {
			msgs = append(msgs, Message{Role: RoleAssistant, Content: ex.Output})
		}
	}

	if user != "" {
		msgs = append(msgs, Message{Role: RoleUser, Content: user})
	}

	return msgs
}
