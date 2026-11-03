// Package jobconfig reads the catalog selection of an analysis job from the
// job's stored config: the pb.JobConfig that the pipeline's CreateJob stores
// with encoding/json. It is the one reader of that selection, shared by the
// pipeline's stages and the results service, so both resolve a config the
// same way.
package jobconfig

import "encoding/json"

// Catalogs is the catalog selection a job's stored config names.
type Catalogs struct {
	// SourceID is the catalog whose controls are mapped (catalog_id).
	SourceID string
	// TargetID is the catalog they are mapped onto (target_catalog_id). It
	// equals SourceID for an analysis within one catalog, including a job
	// whose stored config predates target_catalog_id: such a job ran within
	// its catalog when it was created, and resumes the same way.
	TargetID string
	// Bidirectional adds target-to-source pairs. It is always false within
	// one catalog, whose controls are already paired in both directions.
	Bidirectional bool
	// NoCache is the job's no_cache: it skips result-cache lookups (every
	// LLM call is sent) and still stores results. It is part of the stored
	// config, so the job's attestation records it, and a new-job command
	// built from the config keeps it.
	NoCache bool
	// SourceLabel and TargetLabel are each catalog's name and version as
	// CreateJob recorded them (source_catalog, target_catalog). Within one
	// catalog TargetLabel is SourceLabel. A label is not Recorded in a
	// config stored before snapshots, or when its snapshot names a catalog
	// other than its side's.
	SourceLabel Label
	TargetLabel Label
}

// Label is a catalog's name and version as recorded when the job was
// created. Recorded is false when the config holds none.
type Label struct {
	Name     string
	Version  string
	Recorded bool
}

// LabelOf returns the recorded label of catalogID, one of the job's
// catalogs; the zero Label for any other ID or when none was recorded.
func (c Catalogs) LabelOf(catalogID string) Label {
	switch catalogID {
	case c.SourceID:
		return c.SourceLabel
	case c.TargetID:
		return c.TargetLabel
	}
	return Label{}
}

// CrossCatalog reports whether the job maps one catalog onto another.
func (c Catalogs) CrossCatalog() bool { return c.SourceID != c.TargetID }

// CatalogIDs returns the job's distinct catalogs, source first.
func (c Catalogs) CatalogIDs() []string {
	if c.CrossCatalog() {
		return []string{c.SourceID, c.TargetID}
	}
	return []string{c.SourceID}
}

// snapshot is the stored form of a pb.CatalogSnapshot.
type snapshot struct {
	CatalogID string `json:"catalog_id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

// label returns s as the label of catalogID: not Recorded when s is absent
// or names another catalog.
func (s *snapshot) label(catalogID string) Label {
	if s == nil || s.CatalogID != catalogID {
		return Label{}
	}
	return Label{Name: s.Name, Version: s.Version, Recorded: true}
}

// ExtractCatalogs reads a job's catalogs, direction, cache mode and
// recorded labels from its raw JSON config, which carries the generated
// json tags "catalog_id", "target_catalog_id", "bidirectional", "no_cache",
// "source_catalog" and "target_catalog". It returns ok=false when the
// config is malformed or has no non-empty string catalog_id. CreateJob
// refuses bidirectional within one catalog; a stored config that has it
// anyway runs within the catalog, which already pairs both directions.
func ExtractCatalogs(stored []byte) (Catalogs, bool) {
	var cfg struct {
		CatalogID       string    `json:"catalog_id"`
		TargetCatalogID string    `json:"target_catalog_id"`
		Bidirectional   bool      `json:"bidirectional"`
		NoCache         bool      `json:"no_cache"`
		SourceCatalog   *snapshot `json:"source_catalog"`
		TargetCatalog   *snapshot `json:"target_catalog"`
	}
	if err := json.Unmarshal(stored, &cfg); err != nil || cfg.CatalogID == "" {
		return Catalogs{}, false
	}
	c := Catalogs{SourceID: cfg.CatalogID, TargetID: cfg.TargetCatalogID, Bidirectional: cfg.Bidirectional,
		NoCache: cfg.NoCache}
	if c.TargetID == "" {
		c.TargetID = c.SourceID
	}
	c.SourceLabel = cfg.SourceCatalog.label(c.SourceID)
	c.TargetLabel = cfg.TargetCatalog.label(c.TargetID)
	if !c.CrossCatalog() {
		c.Bidirectional = false
		c.TargetLabel = c.SourceLabel
	}
	return c, true
}
