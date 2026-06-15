package oscal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

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

	var items []ControlItem
	WalkControls(catalog, func(ctrl oscalTypes.Control, groupID string, depth int) {
		params := CollectParams(catalog, ctrl)
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
