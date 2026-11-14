package oscal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type oscalParser struct {
	schemaPath       string
	tracer           trace.Tracer
	requirementParts []string
}

// ParserOption configures a Parser.
type ParserOption func(*oscalParser)

// WithParserTracer sets the OpenTelemetry tracer for the parser.
func WithParserTracer(t trace.Tracer) ParserOption {
	return func(p *oscalParser) { p.tracer = t }
}

// WithRequirementPartNames sets the ordered names of the part that holds a
// control's requirement text (see DecomposeStatements); the default is
// DefaultRequirementPartNames. The slice is copied. Wiring contract: callers
// pass catalog.requirement_part_names (config.CatalogConfig.RequirementPartNames),
// which validation keeps non-empty; an empty list reads no
// part, so every control without a usable title would be textless.
func WithRequirementPartNames(names []string) ParserOption {
	names = slices.Clone(names)
	return func(p *oscalParser) { p.requirementParts = names }
}

// NewParser creates a new OSCAL catalog parser.
// Pass empty schemaPath to skip schema validation.
func NewParser(schemaPath string, opts ...ParserOption) Parser {
	p := &oscalParser{schemaPath: schemaPath, requirementParts: DefaultRequirementPartNames}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *oscalParser) Parse(ctx context.Context, data io.Reader) (*Tree, error) {
	if p.tracer != nil {
		var span trace.Span
		ctx, span = p.tracer.Start(ctx, "oscal.Parse")
		defer span.End()

		tree, err := p.parse(ctx, data)
		if err != nil {
			return nil, err
		}
		span.SetAttributes(
			attribute.Int("control.count", len(tree.Items)),
			attribute.Int("container.count", len(tree.Containers)),
			attribute.Int("control.textless", len(tree.Textless)),
			attribute.String("catalog.format", "oscal-json"),
		)
		return tree, nil
	}
	return p.parse(ctx, data)
}

func (p *oscalParser) parse(ctx context.Context, data io.Reader) (*Tree, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read input: %v", ErrParseFailed, err)
	}

	var root oscalTypes.OscalCompleteSchema
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%w: failed to unmarshal JSON: %v", ErrParseFailed, err)
	}

	if root.Catalog == nil {
		return nil, ErrInvalidFormat
	}

	if p.schemaPath != "" {
		if err := ValidateSchema(raw, p.schemaPath); err != nil {
			return nil, err
		}
	}

	catalog := root.Catalog

	// A statement may insert a parameter its own control does not define: SP
	// 800-53 Rev 5 IA-13(3) inserts its parent's ia-13_odp.01, and SC-42(2)
	// its sibling SC-42(1)'s sc-42.01_odp; without them the text read
	// "[ia-13_odp.01]". OSCAL parameter IDs are unique in a document, so any
	// parameter in the catalog may resolve an insert; the nearest definition
	// wins: the control's own, then its ancestors', then any other control's.
	var everyParam []oscalTypes.Parameter
	WalkControls(catalog, func(ctrl oscalTypes.Control, _, _ string, _ int) {
		if ctrl.Params != nil {
			everyParam = append(everyParam, *ctrl.Params...)
		}
	})
	catalogWide := CollectParams(catalog, oscalTypes.Control{Params: &everyParam})

	var items []ControlItem
	// Every group, with or without an ID, becomes a container keyed by position.
	var containers []Container
	// OSCAL requires a group title, but its JSON schema accepts "". An untitled
	// group is a placeholder, not structure: storing it would put an empty
	// heading into breadcrumbs and prompts, so the import fails closed instead.
	var untitled error
	WalkGroups(catalog, func(group oscalTypes.Group, key, parentKey string) {
		if untitled == nil && strings.TrimSpace(group.Title) == "" {
			untitled = fmt.Errorf("%w: group %q at position %s has no title. "+
				"OSCAL requires every group to have a title. Give the group a title in the catalog, "+
				"or move its controls to the catalog's top level", ErrInvalidFormat, group.ID, key)
		}
		label, _ := selectProp(group.Props, PropLabel)
		containers = append(containers, Container{
			Key:        key,
			Identifier: group.ID,
			Title:      group.Title,
			Label:      label,
			ParentKey:  parentKey,
		})
	})
	if untitled != nil {
		return nil, untitled
	}
	// inherited[d] holds the params of the visited control's depth-d ancestor
	// (WalkControls is pre-order); ancestors come first so a control's own
	// param of the same ID overrides theirs.
	var inherited [][]oscalTypes.Parameter
	// ancestors[d] is the ID of the visited control's depth-d ancestor,
	// truncated by depth like inherited.
	var ancestors []string
	var textless []TextlessControl
	// An item ID is the item's identity in storage, the graph and every report;
	// a control without one, or two items sharing one, cannot be told apart, so
	// the import fails closed on the first such control.
	var idless error
	// origin describes the first item seen with each ID, so a duplicate can
	// name both items; dup is the first duplicate found.
	origin := make(map[string]itemOrigin)
	var dup error
	WalkControls(catalog, func(ctrl oscalTypes.Control, groupID, groupKey string, depth int) {
		if idless == nil && ctrl.ID == "" {
			idless = fmt.Errorf("%w: control titled %q has no id. "+
				"OSCAL requires every control to have a unique id. Give the control an id in the catalog and re-import",
				ErrInvalidFormat, ctrl.Title)
		}
		var own []oscalTypes.Parameter
		if ctrl.Params != nil {
			own = *ctrl.Params
		}
		inherited = append(inherited[:depth], own)
		ancestors = append(ancestors[:depth], ctrl.ID)
		scoped := ctrl
		merged := slices.Concat(inherited...)
		scoped.Params = &merged
		params := collectParams(catalog, scoped, catalogWide)
		decomposed := DecomposeStatements(ctrl, groupID, params, p.requirementParts)
		label, _ := selectProp(ctrl.Props, PropLabel)
		// A withdrawn control is never analysed, so its lack of text is not
		// something the user can or needs to fix.
		withdrawn := withdrawnProps(ctrl)
		if len(withdrawn) == 0 && findRequirementPart(ctrl, p.requirementParts) == nil &&
			UsableTitle(ctrl.Title, ctrl.ID, label) == "" {
			textless = append(textless, TextlessControl{Identifier: ctrl.ID, PartNames: prosePartNames(ctrl)})
		}
		for i := range decomposed {
			decomposed[i].ContainerKey = groupKey
			maps.Copy(decomposed[i].Props, withdrawn)
			if decomposed[i].ID == ctrl.ID {
				decomposed[i].Props[PropNodeKind] = PropNodeKindControl
				if depth > 0 {
					decomposed[i].ParentID = ancestors[depth-1]
				}
			} else {
				decomposed[i].Props[PropNodeKind] = PropNodeKindStatementPart
			}
		}
		for _, item := range decomposed {
			desc := describeItem(item, ctrl.ID)
			if first, seen := origin[item.ID]; seen && dup == nil {
				dup = duplicateIDError(item.ID, first, desc)
			} else if !seen {
				origin[item.ID] = desc
			}
		}
		items = append(items, decomposed...)
	})
	if idless != nil {
		return nil, idless
	}

	if len(items) == 0 {
		return nil, ErrNoControls
	}

	if dup != nil {
		return nil, dup
	}

	return &Tree{Containers: containers, Items: items, Textless: textless}, nil
}

// itemOrigin names an item for a duplicate-ID error. A statement part
// numbered by position, because its source part's id is empty or unpatterned,
// has an ID that appears nowhere in the source, so it also gets a hint saying
// which part to give an id.
type itemOrigin struct {
	name string
	hint string
}

func describeItem(item ControlItem, ctrlID string) itemOrigin {
	if item.Props[PropNodeKind] == PropNodeKindControl {
		return itemOrigin{name: fmt.Sprintf("control %q", item.ID)}
	}
	source := item.Props["source-part"]
	if _, ok := idFromPartID(item.ParentID, source); ok {
		return itemOrigin{name: fmt.Sprintf("statement part %q of control %q (part id %q)", item.ID, ctrlID, source)}
	}
	return itemOrigin{
		name: fmt.Sprintf("statement part %q of control %q", item.ID, ctrlID),
		hint: fmt.Sprintf("%q is not in the source catalog: it was numbered by position from a part of control %q "+
			"whose id (%q) is empty or neither contains \"_smt.\" nor starts with %q; "+
			"give that part an id in the source catalog that starts with %q and re-import. ",
			item.ID, ctrlID, source, item.ParentID+".", item.ParentID+"."),
	}
}

// duplicateIDError reports two items sharing an ID, naming both and, for a
// positionally numbered part, how to give it an ID of its own.
func duplicateIDError(id string, first, second itemOrigin) error {
	fix := "Rename one in the source catalog and re-import"
	if first.hint != "" || second.hint != "" {
		fix = first.hint + second.hint + "Otherwise rename one in the source catalog and re-import"
	}
	return fmt.Errorf("%w: duplicate control ID %q: IDs must be unique across a catalog's controls and statement "+
		"parts, but %s and %s share it. %s", ErrInvalidFormat, id, first.name, second.name, fix)
}

func (p *oscalParser) FindControl(items []ControlItem, controlID string) (*ControlItem, error) {
	return FindControlInSlice(items, controlID)
}
