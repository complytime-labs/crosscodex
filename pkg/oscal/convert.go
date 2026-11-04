package oscal

import (
	"maps"
	"strings"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

const complianceMapperNS = "https://compliance-mapper/ns"

// ControlToItem converts a go-oscal Control to an internal ControlItem.
// Sets ID, Title, GroupID from the go-oscal Control.
// Extracts prose from statement parts only (filter: part.Name == "statement" at top level).
// Within statement parts, recursively collects ALL nested part prose (greedy, regardless of part name).
// If params is non-nil, calls CleanProse on the extracted text.
// Sets Class to ClassRequirement by default.
// Initializes Props map.
func ControlToItem(ctrl oscalTypes.Control, groupID string, params map[string]string) ControlItem {
	item := ControlItem{
		ID:      ctrl.ID,
		Title:   ctrl.Title,
		GroupID: groupID,
		Class:   ClassRequirement,
		Props:   make(map[string]string),
	}

	// Extract prose from statement parts only
	rawText := extractStatementProse(ctrl)

	// Apply parameter substitution if params provided
	if params != nil {
		item.Text = CleanProse(rawText, params)
	} else {
		item.Text = rawText
	}

	return item
}

// extractStatementProse returns empty string if ctrl.Parts is nil.
// Iterates parts, finds name=="statement", collects its Prose.
// Recursively appends prose from ALL nested sub-parts via appendSubpartProse.
func extractStatementProse(ctrl oscalTypes.Control) string {
	if ctrl.Parts == nil {
		return ""
	}

	var collected []string
	for _, part := range *ctrl.Parts {
		if part.Name == "statement" {
			// Collect top-level statement prose
			if part.Prose != "" {
				collected = append(collected, part.Prose)
			}
			// Recursively collect all nested sub-parts
			if part.Parts != nil {
				for _, subPart := range *part.Parts {
					collected = appendSubpartProse(collected, subPart)
				}
			}
		}
	}

	return strings.Join(collected, "\n")
}

// appendSubpartProse recursively appends prose from all nested parts.
// If part.Parts is nil, appends only current part's prose.
// For each sub-part, appends its Prose (if non-empty), then recurses.
func appendSubpartProse(collected []string, part oscalTypes.Part) []string {
	if part.Prose != "" {
		collected = append(collected, part.Prose)
	}

	if part.Parts == nil {
		return collected
	}

	for _, subPart := range *part.Parts {
		collected = appendSubpartProse(collected, subPart)
	}

	return collected
}

// ItemToControl converts a ControlItem back to a go-oscal Control.
// Sets ID, Title.
// If item.Text is non-empty, creates a statement Part with the text.
// If item.Props is non-empty, creates Property slice with namespace compliance-mapper.
func ItemToControl(item ControlItem) oscalTypes.Control {
	ctrl := oscalTypes.Control{
		ID:    item.ID,
		Title: item.Title,
	}

	// Create statement part if text exists
	if item.Text != "" {
		parts := []oscalTypes.Part{
			{
				Name:  "statement",
				Prose: item.Text,
			},
		}
		ctrl.Parts = &parts
	}

	// Convert props to OSCAL properties
	if len(item.Props) > 0 {
		props := make([]oscalTypes.Property, 0, len(item.Props))
		for name, value := range item.Props {
			props = append(props, oscalTypes.Property{
				Name:  name,
				Ns:    complianceMapperNS,
				Value: value,
			})
		}
		ctrl.Props = &props
	}

	return ctrl
}

// CollectParams builds a parameter lookup map from catalog-level and control-level params.
// Resolution order: param.Label first, then param.Values[0], then the
// param.Select choices, else skip. Control params override catalog params.
//
// A selection (SP 800-53 Rev 5 organization-defined parameters such as
// AC-11's) has neither label nor values; without its choices the statement
// reads "by [ac-11_odp.01]" and loses its meaning. It renders as NIST prints
// it, "[Selection (one or more): a; b]". Choices may insert other parameters,
// so selections render after every label and value is known; a choice that
// inserts a selection (or itself) shows that parameter's ID instead of
// recursing.
func CollectParams(catalog *oscalTypes.Catalog, ctrl oscalTypes.Control) map[string]string {
	params := make(map[string]string)
	selections := make(map[string]*oscalTypes.ParameterSelection)

	collect := func(list *[]oscalTypes.Parameter) {
		if list == nil {
			return
		}
		for _, param := range *list {
			if value := resolveParamValue(param); value != "" {
				params[param.ID] = value
				delete(selections, param.ID)
			} else if param.Select != nil && param.Select.Choice != nil && len(*param.Select.Choice) > 0 {
				selections[param.ID] = param.Select
				delete(params, param.ID)
			}
		}
	}

	// Collect catalog-level params first; control params override them.
	if catalog != nil {
		collect(catalog.Params)
	}
	collect(ctrl.Params)

	plain := maps.Clone(params)
	for id, sel := range selections {
		choices := make([]string, 0, len(*sel.Choice))
		for _, choice := range *sel.Choice {
			choices = append(choices, CleanProse(choice, plain))
		}
		prefix := "Selection: "
		if sel.HowMany == "one-or-more" {
			prefix = "Selection (one or more): "
		}
		params[id] = "[" + prefix + strings.Join(choices, "; ") + "]"
	}

	return params
}

// resolveParamValue extracts value from Parameter using label-first, then values[0].
func resolveParamValue(param oscalTypes.Parameter) string {
	// Prefer label
	if param.Label != "" {
		return param.Label
	}

	// Fallback to first value
	if param.Values != nil && len(*param.Values) > 0 {
		return (*param.Values)[0]
	}

	return ""
}
