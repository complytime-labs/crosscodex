package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"

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
// controls, is refused before anything is stored. Output whose elements name
// their own controls and groups skips the Structurer and is imported as given;
// the returned meta then carries the catalog's title and version. The output
// must name requestedMapping (an output naming none is ""), else the import
// is refused before any tier runs; prov.ContentHash becomes the output's
// stable content hash. An output that carries a structure method (schema 1.4)
// is counted and traced as "ingestion_<method>", and when ingestion guessed
// the structure it is refused while catalog.structuring has cascade-only
// settings off their defaults.
func (s *Service) structureIngestion(ctx context.Context, prov *oscal.Provenance, documentID, requestedMapping string, data []byte) (*oscal.Tree, IngestionMeta, *connect.Error) {
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
		return nil, meta, s.internalError(ctx, "ParseCatalog", fmt.Errorf("adapt ingestion output %q: %w", documentID, err))
	}
	if meta.MappingID != requestedMapping {
		count("", "mapping_mismatch")
		return nil, meta, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"cannot import document %q: the import asked for mapping %.64q but the converted output names mapping %.64q. "+
				"Nothing was stored; re-run the conversion, and if it recurs report it as an ingestion service bug",
			documentID, requestedMapping, meta.MappingID))
	}
	// An output that carries a method is labelled by it, and a guessed
	// structure (any method but mapping) is refused when settings that only
	// tuned the cascade were changed: ingestion already did that work.
	var label oscal.StructureTier
	if meta.StructureMethod != "" {
		label = oscal.StructureTier("ingestion_" + meta.StructureMethod)
	}
	// Not mapping means heading, table or pattern: checkStructureMethod refuses any
	// other value, so a new method must be added there first.
	if meta.StructureMethod != "" && meta.StructureMethod != ingestionMethodMapping {
		changed, err := s.changedCascadeSettings()
		if err != nil {
			count(label, "failed")
			return nil, meta, s.internalError(ctx, "ParseCatalog", fmt.Errorf("compare catalog.structuring with its defaults: %w", err))
		}
		if len(changed) > 0 {
			count(label, "refused")
			trace.SpanFromContext(ctx).SetAttributes(attribute.String("structure.tier", string(label)))
			return nil, meta, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"cannot import document %q: ingestion already structured it (method %s), so these catalog.structuring settings no longer apply: %s. "+
					"Keep crosscodex-ingestion below schema 1.4 until document mappings exist (crosscodex-ingestion#38 phase 3), "+
					"or reset (or ask an operator to reset) those settings to their defaults to accept ingestion's structure. Nothing was stored",
				documentID, meta.StructureMethod, strings.Join(changed, ", ")))
		}
	}
	items := meta.SourceItems
	tier := oscal.StructureTierSourceIDs
	if items == nil {
		items, tier, err = s.structurer.StructureWithTier(ctx, doc, s.structureOpts)
		if err != nil {
			count(tier, "failed")
			return nil, meta, s.internalError(ctx, "ParseCatalog", fmt.Errorf("structure ingestion output %q: %w", documentID, err))
		}
	}

	if label == "" {
		label = tier
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("structure.tier", string(label)))
	switch tier {
	case oscal.StructureTierFallback:
		count(label, "refused")
		return nil, meta, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"cannot import document %q: structuring reached the %q tier, which splits paragraphs instead of finding controls, "+
				"because no heading, table or numbering pattern in the converted document matched and no LLM tier produced controls. "+
				"Set (or ask an operator to set) catalog.structuring.section_pattern to a regular expression matching the document's control identifiers "+
				"(group 1 the identifier, group 2 the text), then import again. Nothing was stored", documentID, tier))
	case oscal.StructureTierLLMDetect, oscal.StructureTierLLMExtract:
		s.logger.InfoContext(ctx, "converted document structured by an LLM tier",
			"document_id", documentID, "tier", tier, "items", len(items))
	case oscal.StructureTierSourceIDs:
		groups := map[string]bool{}
		sections := 0
		for _, item := range items {
			if item.GroupID != "" {
				groups[item.GroupID] = true
			}
			if item.Class == oscal.ClassSection {
				sections++
			}
		}
		s.logger.InfoContext(ctx, "converted document imported by source IDs",
			"document_id", documentID, "controls", len(items)-sections, "sections", sections, "groups", len(groups))
	}

	tree, err := oscal.BuildTree(items)
	if err != nil {
		count(label, "invalid_items")
		if tier == oscal.StructureTierSourceIDs && meta.StructureMethod != "" && meta.StructureMethod != ingestionMethodMapping {
			// A guessed structure has no mapping to fix, and a re-run gives the
			// same output.
			return nil, meta, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
				"cannot import document %q: the structure ingestion guessed (method %s) does not form a catalog tree: %v. "+
					"Nothing was stored; report it as an ingestion service bug. A document mapping (planned, crosscodex-ingestion#38 phase 3) "+
					"will let the document declare its own structure",
				documentID, meta.StructureMethod, err))
		}
		if tier == oscal.StructureTierSourceIDs {
			// The IDs and groups are the converter's, not a cascade result, so
			// section_pattern and decompose cannot change them.
			return nil, meta, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
				"cannot import document %q: its source-declared controls do not form a catalog tree: %v. "+
					"The converter or its mapping declared these IDs and groups; re-run the conversion or fix the mapping, then import again. Nothing was stored",
				documentID, err))
		}
		return nil, meta, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"cannot import document %q: its %q tier controls do not form a catalog tree: %v. "+
				"Set (or ask an operator to set) catalog.structuring.section_pattern to a regular expression matching the document's control identifiers, "+
				"or, if the clash is between decomposed sub-items (IDs with a \".\" suffix), set catalog.structuring.decompose to false; then import again. "+
				"This applies only to ingestion output below schema 1.4; from 1.4 the converter structures the document itself. Nothing was stored",
			documentID, tier, err))
	}
	count(label, "structured")
	prov.Format = FormatIngestion
	prov.SourceURI = documentID
	prov.ContentHash = meta.ContentHash
	prov.SetExtractor(IngestionExtractorName, meta.SchemaVersion)
	return tree, meta, nil
}

// changedCascadeSettings names, in the documented order, the catalog.structuring
// settings that only tune the pre-1.4 cascade and differ from their
// pkg/config defaults. A Service built without WithStructuring has the zero
// config, which differs from the defaults, so every guessed 1.4 import is
// refused (fail closed); composition roots always pass WithStructuring. The defaults come from pkg/config so they cannot drift
// from the shipped configuration.
func (s *Service) changedCascadeSettings() ([]string, error) {
	def, err := config.DefaultStructuringConfig()
	if err != nil {
		return nil, err
	}
	o := s.structureOpts
	var changed []string
	for _, c := range []struct {
		name string
		diff bool
	}{
		{"section_pattern", o.SectionPattern != def.SectionPattern},
		{"decompose", o.Decompose != def.Decompose},
		{"min_decompose_words", o.MinDecomposeWords != def.MinDecomposeWords},
		{"filter_by_keywords", o.FilterByKeywords != def.FilterByKeywords},
		{"keywords", !slices.Equal(o.Keywords, def.Keywords)},
		{"max_heading_repeats", o.MaxHeadingRepeats != def.MaxHeadingRepeats},
		{"print_artifact_patterns", !slices.Equal(o.PrintArtifactPatterns, def.PrintArtifactPatterns)},
	} {
		if c.diff {
			changed = append(changed, c.name)
		}
	}
	return changed, nil
}
