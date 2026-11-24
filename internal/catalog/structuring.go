package catalog

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// IngestionExtractorName is the provenance extractor name of a catalog
// structured from crosscodex-ingestion output. Its extractor version is the
// output's schema_version: schema v1 output does not name the converter
// version.
const IngestionExtractorName = "crosscodex-ingestion"

// WithStructurer sets the Structurer that turns converted documents
// (crosscodex-ingestion output) into controls. Without one, ParseCatalog
// does not treat any document as converted output, so ingestion-shaped JSON
// gets the same UnsupportedFormatError as any other non-OSCAL document.
// Wiring contract: cmd/crosscodexd and the embedded CLI pass
// oscal.NewStructurer with no Completer (the LLM tiers never run), and only
// when ingestion.address is set; it has no pkg/config field of its own
// (catalog.structuring.* feeds WithStructuring).
func WithStructurer(st oscal.Structurer) ServiceOption {
	return func(s *Service) {
		s.structurer = st
	}
}

// WithStructuring sets the options converted documents are structured with,
// from config catalog.structuring.
func WithStructuring(cfg config.StructuringConfig) ServiceOption {
	return func(s *Service) {
		s.structureOpts = oscal.StructureOptions{
			SectionPattern:        cfg.SectionPattern,
			Decompose:             cfg.Decompose,
			MinDecomposeWords:     cfg.MinDecomposeWords,
			FilterByKeywords:      cfg.FilterByKeywords,
			Keywords:              cfg.Keywords,
			ChunkChars:            cfg.ChunkChars,
			MaxValidationChars:    cfg.MaxValidationChars,
			AllowedFormats:        cfg.AllowedFormats,
			MaxHeadingRepeats:     cfg.MaxHeadingRepeats,
			PrintArtifactPatterns: cfg.PrintArtifactPatterns,
		}
	}
}

// structureIngestion turns stored crosscodex-ingestion output into a tree and
// records its provenance on prov. It fails closed: output it cannot read is an
// internal error (the converter, not the caller, produced it), and a result
// from the fallback tier, which splits paragraphs rather than finding
// controls, is refused before anything is stored.
func (s *Service) structureIngestion(ctx context.Context, prov *oscal.Provenance, documentID string, data []byte) (*oscal.Tree, *connect.Error) {
	// Every outcome is counted by the tier that ran ("none" before one did)
	// and its result; "structured" means a tree was built, before ParseCatalog
	// validates and stores it.
	count := func(tier oscal.StructureTier, result string) {
		if tier == "" {
			tier = "none"
		}
		if s.structuredTotal != nil {
			s.structuredTotal.Add(ctx, 1, metric.WithAttributes(
				attribute.String("tier", string(tier)), attribute.String("result", result)))
		}
	}
	doc, meta, err := AdaptIngestionOutput(data)
	if err != nil {
		count("", "invalid_output")
		return nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("adapt ingestion output %q: %w", documentID, err))
	}
	items, tier, err := s.structurer.StructureWithTier(ctx, doc, s.structureOpts)
	if err != nil {
		count(tier, "failed")
		return nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("structure ingestion output %q: %w", documentID, err))
	}

	trace.SpanFromContext(ctx).SetAttributes(attribute.String("structure.tier", string(tier)))
	switch tier {
	case oscal.StructureTierFallback:
		count(tier, "refused")
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"cannot import document %q: structuring reached the %q tier, which splits paragraphs instead of finding controls, "+
				"because no heading, table or numbering pattern in the converted document matched and no LLM tier produced controls. "+
				"Set (or ask an operator to set) catalog.structuring.section_pattern to a regular expression matching the document's control identifiers "+
				"(group 1 the identifier, group 2 the text), then import again. Nothing was stored", documentID, tier))
	case oscal.StructureTierLLMDetect, oscal.StructureTierLLMExtract:
		s.logger.InfoContext(ctx, "converted document structured by an LLM tier",
			"document_id", documentID, "tier", tier, "items", len(items))
	}

	tree, err := oscal.BuildTree(items)
	if err != nil {
		count(tier, "invalid_items")
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"cannot import document %q: its %q tier controls do not form a catalog tree: %v. "+
				"Set (or ask an operator to set) catalog.structuring.section_pattern to a regular expression matching the document's control identifiers, "+
				"or, if the clash is between decomposed sub-items (IDs with a \".\" suffix), set catalog.structuring.decompose to false; then import again. Nothing was stored",
			documentID, tier, err))
	}
	count(tier, "structured")
	prov.Format = FormatIngestion
	prov.SourceURI = documentID
	prov.SetExtractor(IngestionExtractorName, meta.SchemaVersion)
	return tree, nil
}
