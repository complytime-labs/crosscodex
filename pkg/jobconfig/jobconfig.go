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

// ExtractCatalogs reads a job's catalogs and direction from its raw JSON
// config, which carries the generated json tags "catalog_id",
// "target_catalog_id" and "bidirectional". It returns ok=false when the
// config is malformed or has no non-empty string catalog_id. CreateJob
// refuses bidirectional within one catalog; a stored config that has it
// anyway runs within the catalog, which already pairs both directions.
func ExtractCatalogs(stored []byte) (Catalogs, bool) {
	var cfg struct {
		CatalogID       string `json:"catalog_id"`
		TargetCatalogID string `json:"target_catalog_id"`
		Bidirectional   bool   `json:"bidirectional"`
	}
	if err := json.Unmarshal(stored, &cfg); err != nil || cfg.CatalogID == "" {
		return Catalogs{}, false
	}
	c := Catalogs{SourceID: cfg.CatalogID, TargetID: cfg.TargetCatalogID, Bidirectional: cfg.Bidirectional}
	if c.TargetID == "" {
		c.TargetID = c.SourceID
	}
	if !c.CrossCatalog() {
		c.Bidirectional = false
	}
	return c, true
}
