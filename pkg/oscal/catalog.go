package oscal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type oscalParser struct {
	schemaPath string
	tracer     trace.Tracer
}

// ParserOption configures a Parser.
type ParserOption func(*oscalParser)

// WithParserTracer sets the OpenTelemetry tracer for the parser.
func WithParserTracer(t trace.Tracer) ParserOption {
	return func(p *oscalParser) { p.tracer = t }
}

// NewParser creates a new OSCAL catalog parser.
// Pass empty schemaPath to skip schema validation.
func NewParser(schemaPath string, opts ...ParserOption) Parser {
	p := &oscalParser{schemaPath: schemaPath}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *oscalParser) Parse(ctx context.Context, data io.Reader) ([]ControlItem, error) {
	if p.tracer != nil {
		var span trace.Span
		ctx, span = p.tracer.Start(ctx, "oscal.Parse")
		defer span.End()

		items, err := p.parse(ctx, data)
		if err != nil {
			return nil, err
		}
		span.SetAttributes(
			attribute.Int("control.count", len(items)),
			attribute.String("catalog.format", "oscal-json"),
		)
		return items, nil
	}
	return p.parse(ctx, data)
}

func (p *oscalParser) parse(ctx context.Context, data io.Reader) ([]ControlItem, error) {
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
	WalkControls(catalog, func(ctrl oscalTypes.Control, _ string, _ int) {
		if ctrl.Params != nil {
			everyParam = append(everyParam, *ctrl.Params...)
		}
	})
	catalogWide := CollectParams(catalog, oscalTypes.Control{Params: &everyParam})

	var items []ControlItem
	// inherited[d] holds the params of the visited control's depth-d ancestor
	// (WalkControls is pre-order); ancestors come first so a control's own
	// param of the same ID overrides theirs.
	var inherited [][]oscalTypes.Parameter
	WalkControls(catalog, func(ctrl oscalTypes.Control, groupID string, depth int) {
		var own []oscalTypes.Parameter
		if ctrl.Params != nil {
			own = *ctrl.Params
		}
		inherited = append(inherited[:depth], own)
		scoped := ctrl
		merged := slices.Concat(inherited...)
		scoped.Params = &merged
		params := collectParams(catalog, scoped, catalogWide)
		decomposed := DecomposeStatements(ctrl, groupID, params)
		items = append(items, decomposed...)
	})

	if len(items) == 0 {
		return nil, ErrNoControls
	}

	return items, nil
}

func (p *oscalParser) FindControl(items []ControlItem, controlID string) (*ControlItem, error) {
	return FindControlInSlice(items, controlID)
}
