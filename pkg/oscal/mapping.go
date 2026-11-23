package oscal

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/defenseunicorns/go-oscal/src/pkg/validation"
	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-2-3"
	"github.com/google/uuid"
)

// crosscodexUUIDNamespace is the UUIDv5 namespace of every UUID in an exported
// mapping-collection. Changing it changes every exported UUID, which breaks
// diffing against earlier exports.
var crosscodexUUIDNamespace = uuid.MustParse("23a0972b-ec85-4cd0-b49a-a4d07089a330")

const (
	mappingOSCALVersion = "1.2.3"
	// maxReportedSchemaErrors bounds how many validator errors an
	// ErrMappingExport message quotes.
	maxReportedSchemaErrors = 3
)

// Prop is a name/value pair, e.g. one export filter.
type Prop struct{ Name, Value string }

// MappingExport is everything one job export needs, in CrossCodex terms.
// BuildMappingCollection is the only OSCAL-aware step, so callers stay
// independent of the OSCAL mapping model.
type MappingExport struct {
	JobID string
	Stage string // "synthesized" | "analyzer"
	// Filters are recorded verbatim as filter-<Name> provenance props.
	Filters []Prop
	// CompletedAt is the latest stage completion; it becomes last-modified.
	CompletedAt time.Time
	Source      ExportCatalog
	Target      ExportCatalog
	Models      []ExportModel
	Mappings    []ExportMapping
}

// ExportCatalog identifies one side of the mapping by its content-derived ID.
type ExportCatalog struct{ ID, Name, Version, VersionDisplay string }

// ExportModel names the model behind an alias; Revision may be empty.
type ExportModel struct{ Alias, Observed, Revision string }

// ExportMapping is one stored mapping row. Control IDs are catalog-qualified
// ("<catalogID>/<OSCAL id>"); Relationship is the CrossCodex name.
type ExportMapping struct {
	MappingID, SourceControlID, TargetControlID string
	Relationship                                string  // EQUIVALENT ... NO_RELATIONSHIP
	Confidence                                  float64 // 0..1
	CleaningState                               string  // "", "restored", "stale"
	CritiqueOutcome                             string  // "" or upheld|split|rejected|failed
}

// relationshipSpec is the OSCAL spelling of a CrossCodex relationship. The
// OSCAL mapping model only defines the relationship values in its own
// namespace, so the other CrossCodex types carry complianceMapperNS.
type relationshipSpec struct {
	name  string
	oscal bool
}

var mappingRelationships = map[string]relationshipSpec{
	"EQUIVALENT":      {"equivalent-to", true},
	"SUPERSET_OF":     {"superset-of", true},
	"SUBSET_OF":       {"subset-of", true},
	"NO_RELATIONSHIP": {"no-relationship", true},
	"CONTRIBUTES_TO":  {"contributes-to", false},
	"COMPLEMENTS":     {"complements", false},
	"PARTIAL":         {"partial", false},
	"CONFLICTS_WITH":  {"conflicts-with", false},
}

// The 1.2.3 mapping model, redefined where go-oscal's types cannot express it:
// its ConfidenceScore drops a zero percentage (omitempty) and its
// MappingCollection holds a single Mapping.
type mappingCollectionDoc struct {
	MappingCollection mappingCollection `json:"mapping-collection"`
}

type mappingCollection struct {
	UUID       string                       `json:"uuid"`
	Metadata   oscalTypes.Metadata          `json:"metadata"`
	Provenance oscalTypes.MappingProvenance `json:"provenance"`
	Mappings   []mapping                    `json:"mappings"`
	BackMatter *oscalTypes.BackMatter       `json:"back-matter,omitempty"`
}

type mapping struct {
	UUID           string                              `json:"uuid"`
	SourceResource oscalTypes.MappingResourceReference `json:"source-resource"`
	TargetResource oscalTypes.MappingResourceReference `json:"target-resource"`
	Maps           []mapEntry                          `json:"maps"`
}

type mapEntry struct {
	UUID            string                   `json:"uuid"`
	Relationship    string                   `json:"relationship"`
	Ns              string                   `json:"ns,omitempty"`
	Sources         []oscalTypes.MappingItem `json:"sources"`
	Targets         []oscalTypes.MappingItem `json:"targets"`
	ConfidenceScore confidenceScore          `json:"confidence-score"`
	Props           *[]oscalTypes.Property   `json:"props,omitempty"`
}

type confidenceScore struct {
	Percentage float64 `json:"percentage"`
}

// builtMap is a mapEntry plus the keys it is ordered by.
type builtMap struct {
	entry             mapEntry
	sourceRef, tgtRef string
	mappingID         string
}

// BuildMappingCollection returns the OSCAL 1.2.3 mapping-collection JSON for a
// job export. The result is deterministic and schema-validated; on any error it
// returns nil bytes and an error wrapping ErrMappingExport.
func BuildMappingCollection(in MappingExport) ([]byte, error) {
	if len(in.Mappings) == 0 {
		return nil, fmt.Errorf("%w: job %s has no mappings to export with these filters (retired rows are excluded from OSCAL exports); widen the filters or export --format json to see every row", ErrMappingExport, in.JobID)
	}
	if in.CompletedAt.IsZero() {
		return nil, fmt.Errorf("%w: job %s has no completion time for last-modified; export it after it completes", ErrMappingExport, in.JobID)
	}

	srcPrefix, tgtPrefix := in.Source.ID+"/", in.Target.ID+"/"
	var forward, reverse []builtMap
	for _, row := range in.Mappings {
		bm, err := buildMapEntry(row)
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(row.SourceControlID, srcPrefix) && strings.HasPrefix(row.TargetControlID, tgtPrefix):
			bm.sourceRef = strings.TrimPrefix(row.SourceControlID, srcPrefix)
			bm.tgtRef = strings.TrimPrefix(row.TargetControlID, tgtPrefix)
			forward = append(forward, bm)
		case strings.HasPrefix(row.SourceControlID, tgtPrefix) && strings.HasPrefix(row.TargetControlID, srcPrefix):
			bm.sourceRef = strings.TrimPrefix(row.SourceControlID, tgtPrefix)
			bm.tgtRef = strings.TrimPrefix(row.TargetControlID, srcPrefix)
			reverse = append(reverse, bm)
		default:
			return nil, fmt.Errorf("%w: mapping %s has control IDs %q and %q; each must start with %q or %q (one per catalog)", ErrMappingExport, row.MappingID, row.SourceControlID, row.TargetControlID, srcPrefix, tgtPrefix)
		}
	}

	// A single-catalog job (source and target share an ID) has one resource
	// that both sides reference; its rows all match the forward case above.
	srcResource := catalogResource(in.Source)
	tgtResource := srcResource
	resources := []oscalTypes.Resource{srcResource}
	if in.Source.ID != in.Target.ID {
		tgtResource = catalogResource(in.Target)
		resources = append(resources, tgtResource)
	}
	doc := mappingCollectionDoc{MappingCollection: mappingCollection{
		Metadata: oscalTypes.Metadata{
			Title:        fmt.Sprintf("CrossCodex mapping: %s to %s", singleLine(in.Source.Name), singleLine(in.Target.Name)),
			LastModified: in.CompletedAt.UTC(),
			Version:      in.JobID,
			OscalVersion: mappingOSCALVersion,
		},
		Provenance: oscalTypes.MappingProvenance{
			Method:             "automation",
			MatchingRationale:  "semantic",
			Status:             "draft",
			MappingDescription: fmt.Sprintf("CrossCodex job %s: %s to %s, %s stage. Unreviewed machine output.", in.JobID, catalogDisplay(in.Source), catalogDisplay(in.Target), in.Stage),
			Props:              provenanceProps(in),
		},
		BackMatter: &oscalTypes.BackMatter{Resources: &resources},
	}}
	if len(forward) > 0 {
		doc.MappingCollection.Mappings = append(doc.MappingCollection.Mappings,
			assembleMapping(in.JobID, "forward", srcResource.UUID, tgtResource.UUID, forward))
	}
	if len(reverse) > 0 {
		doc.MappingCollection.Mappings = append(doc.MappingCollection.Mappings,
			assembleMapping(in.JobID, "reverse", tgtResource.UUID, srcResource.UUID, reverse))
	}

	// The document UUID hashes the document with a blank UUID so identical
	// content always yields identical bytes and any change yields a new UUID.
	blank, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: encode document: %w", ErrMappingExport, err)
	}
	sum := sha256.Sum256(blank)
	doc.MappingCollection.UUID = uuid.NewSHA1(crosscodexUUIDNamespace, sum[:]).String()
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: encode document: %w", ErrMappingExport, err)
	}
	if err := validateMappingCollection(out); err != nil {
		return nil, err
	}
	return out, nil
}

// buildMapEntry converts one row, validating its relationship and confidence.
func buildMapEntry(row ExportMapping) (builtMap, error) {
	rel, ok := mappingRelationships[row.Relationship]
	if !ok {
		return builtMap{}, fmt.Errorf("%w: mapping %s has unknown relationship %q; this CrossCodex version cannot express it in OSCAL, export --format json instead", ErrMappingExport, row.MappingID, row.Relationship)
	}
	if math.IsNaN(row.Confidence) || row.Confidence < 0 || row.Confidence > 1 {
		return builtMap{}, fmt.Errorf("%w: mapping %s has confidence %v outside 0..1; re-run the analysis or export --format json", ErrMappingExport, row.MappingID, row.Confidence)
	}
	props := []oscalTypes.Property{mapperProp("mapping-id", row.MappingID)}
	if row.CleaningState != "" {
		props = append(props, mapperProp("cleaning-state", row.CleaningState))
	}
	if row.CritiqueOutcome != "" {
		props = append(props, mapperProp("critique-outcome", row.CritiqueOutcome))
	}
	entry := mapEntry{
		UUID:            uuid.NewSHA1(crosscodexUUIDNamespace, []byte("map\x00"+row.MappingID)).String(),
		Relationship:    rel.name,
		ConfidenceScore: confidenceScore{Percentage: math.Round(row.Confidence*1e4) / 1e4},
		Props:           &props,
	}
	if !rel.oscal {
		entry.Ns = complianceMapperNS
	}
	return builtMap{entry: entry, mappingID: row.MappingID}, nil
}

// assembleMapping orders one direction's maps and wraps them in a mapping.
func assembleMapping(jobID, direction, sourceResourceUUID, targetResourceUUID string, maps []builtMap) mapping {
	slices.SortFunc(maps, func(a, b builtMap) int {
		return strings.Compare(a.sourceRef+"\x00"+a.tgtRef+"\x00"+a.mappingID,
			b.sourceRef+"\x00"+b.tgtRef+"\x00"+b.mappingID)
	})
	m := mapping{
		UUID:           uuid.NewSHA1(crosscodexUUIDNamespace, []byte("mapping\x00"+jobID+"\x00"+direction)).String(),
		SourceResource: oscalTypes.MappingResourceReference{Type: "catalog", Href: "#" + sourceResourceUUID},
		TargetResource: oscalTypes.MappingResourceReference{Type: "catalog", Href: "#" + targetResourceUUID},
	}
	for _, bm := range maps {
		bm.entry.Sources = []oscalTypes.MappingItem{{Type: "control", IDRef: bm.sourceRef}}
		bm.entry.Targets = []oscalTypes.MappingItem{{Type: "control", IDRef: bm.tgtRef}}
		m.Maps = append(m.Maps, bm.entry)
	}
	return m
}

func provenanceProps(in MappingExport) *[]oscalTypes.Property {
	props := []oscalTypes.Property{mapperProp("job-id", in.JobID), mapperProp("stage", in.Stage)}
	for _, f := range in.Filters {
		props = append(props, mapperProp("filter-"+f.Name, f.Value))
	}
	for _, m := range in.Models {
		value := m.Alias + "=" + m.Observed
		if m.Revision != "" {
			value += "@" + m.Revision
		}
		props = append(props, mapperProp("model", value))
	}
	return &props
}

func catalogResource(c ExportCatalog) oscalTypes.Resource {
	props := []oscalTypes.Property{mapperProp("catalog-id", c.ID)}
	if c.VersionDisplay != "" {
		props = append(props, mapperProp("version-display", c.VersionDisplay))
	}
	return oscalTypes.Resource{
		UUID:  uuid.NewSHA1(crosscodexUUIDNamespace, []byte("resource\x00"+c.ID)).String(),
		Title: catalogDisplay(c),
		Props: &props,
	}
}

// catalogDisplay is the single-line name plus " <Version>" when set;
// VersionDisplay is kept as a prop only.
func catalogDisplay(c ExportCatalog) string {
	name := singleLine(c.Name)
	if c.Version != "" {
		return name + " " + singleLine(c.Version)
	}
	return name
}

// singleLine replaces each run of line breaks with one space. The schema's
// title and description types forbid line breaks, and catalog names are
// uploader-controlled, so the document flattens them; the exact input stays
// identifiable through the catalog-id prop.
func singleLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }), " ")
}

func mapperProp(name, value string) oscalTypes.Property {
	return oscalTypes.Property{Name: name, Value: value, Ns: complianceMapperNS}
}

// validateMappingCollection checks the encoded document against the embedded
// OSCAL 1.2.3 schema, quoting at most maxReportedSchemaErrors problems.
func validateMappingCollection(doc []byte) error {
	// Pass a decoded map, not bytes: go-oscal parses bytes as YAML, which
	// rejects control characters that are legal inside JSON strings.
	var model map[string]any
	if err := json.Unmarshal(doc, &model); err != nil {
		return fmt.Errorf("%w: decode document for validation: %w", ErrMappingExport, err)
	}
	v, err := validation.NewValidatorDesiredVersion(model, mappingOSCALVersion)
	if err != nil {
		return fmt.Errorf("%w: prepare schema validation: %w", ErrMappingExport, err)
	}
	if err := v.Validate(); err != nil {
		var msgs []string
		if res, resErr := v.GetValidationResult(); resErr == nil {
			for _, e := range res.Errors {
				if len(msgs) == maxReportedSchemaErrors {
					break
				}
				msgs = append(msgs, e.InstanceLocation+": "+e.Error)
			}
		}
		if len(msgs) == 0 {
			msgs = append(msgs, err.Error())
		}
		return fmt.Errorf("%w: document failed OSCAL %s schema validation: %s; a recorded value (a catalog version, model name or filter) is not valid in OSCAL. Export --format json instead, and report this", ErrMappingExport, mappingOSCALVersion, strings.Join(msgs, "; "))
	}
	return nil
}
