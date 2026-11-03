package main

import (
	"context"
	"fmt"

	connectrpc "connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
)

// Directions a job pairs its catalogs' controls in, as run status reports
// them.
const (
	directionWithinCatalog  = "within_catalog"
	directionSourceToTarget = "source_to_target"
	directionBidirectional  = "bidirectional"
)

// directionText is the human-readable form of each direction.
var directionText = map[string]string{
	directionWithinCatalog:  "within one catalog (its controls paired in both directions)",
	directionSourceToTarget: "source to target",
	directionBidirectional:  "both ways (source to target, and target to source)",
}

// maxUntrustedDisplayBytes bounds an uploader-controlled value (a catalog
// name, version or ID, a control ID) on one output line or CSV cell.
const maxUntrustedDisplayBytes = 200

// maxLookupErrorBytes bounds the reason a catalog lookup failed, as run
// status shows it.
const maxLookupErrorBytes = 160

// catalogRef is one catalog a job analyses. Name and Version are the ones
// the job recorded at creation when Snapshot is true; otherwise (a job
// created before snapshots) they are the catalog's current ones from
// GetCatalog. Found and ControlCount come from GetCatalog: Found is false
// when the lookup did not return the catalog; on NotFound (deleted or purged
// after the job was created, or not visible to the caller's tenant)
// LookupError is empty, and on any other failure it holds the cleaned reason.
type catalogRef struct {
	ID           string
	Name         string
	Version      string
	ControlCount int32
	Found        bool
	LookupError  string
	Snapshot     bool
}

// withSnapshot returns r with snap's name and version when snap is r's
// catalog's snapshot; r unchanged otherwise.
func (r catalogRef) withSnapshot(snap *pb.CatalogSnapshot) catalogRef {
	if snap == nil || snap.GetCatalogId() != r.ID {
		return r
	}
	r.Name, r.Version, r.Snapshot = snap.GetName(), snap.GetVersion(), true
	return r
}

// jobCatalogs is a job's catalog selection, read for run status, so a reader
// sees which catalogs (and which versions) a job maps, and how many controls
// each side has, without decoding IDs.
type jobCatalogs struct {
	Source    catalogRef
	Target    catalogRef
	Direction string
}

// readJobCatalogs looks up the catalogs cfg names. It returns nil for a
// config that names no catalog. A config without target_catalog_id predates
// cross-catalog analysis and ran within its one catalog, as the pipeline
// runs it. tenant, when set, is passed as the lookups' tenant context. A
// failed lookup is recorded on that catalog, not returned: the job was read,
// so run status still shows it.
func readJobCatalogs(ctx context.Context, client crosscodexv1connect.GatewayServiceClient, cfg *pb.JobConfig, tenant string) *jobCatalogs {
	sourceID := cfg.GetCatalogId()
	if sourceID == "" {
		return nil
	}
	targetID := cfg.GetTargetCatalogId()
	if targetID == "" {
		targetID = sourceID
	}

	jc := &jobCatalogs{Direction: directionSourceToTarget}
	switch {
	case targetID == sourceID:
		jc.Direction = directionWithinCatalog
	case cfg.GetBidirectional():
		jc.Direction = directionBidirectional
	}

	jc.Source = lookupCatalog(ctx, client, sourceID, tenant).withSnapshot(cfg.GetSourceCatalog())
	if targetID == sourceID {
		jc.Target = jc.Source
		return jc
	}
	jc.Target = lookupCatalog(ctx, client, targetID, tenant).withSnapshot(cfg.GetTargetCatalog())
	return jc
}

// lookupCatalog describes catalog id. A NotFound answer is not a failure:
// the job still ran over that catalog, so it is shown by ID with Found false.
// Any other error is kept, cleaned to one short line, in LookupError.
func lookupCatalog(ctx context.Context, client crosscodexv1connect.GatewayServiceClient, id, tenant string) catalogRef {
	req := &pb.GetCatalogRequest{CatalogId: id}
	if tenant != "" {
		req.TenantContext = &pb.TenantContext{TenantId: tenant}
	}
	resp, err := client.GetCatalog(ctx, connectrpc.NewRequest(req))
	if err != nil {
		if connectrpc.CodeOf(err) == connectrpc.CodeNotFound {
			return catalogRef{ID: id}
		}
		return catalogRef{ID: id, LookupError: intanalyzer.CleanDisplay(err.Error(), maxLookupErrorBytes)}
	}
	c := resp.Msg.GetCatalog()
	return catalogRef{ID: id, Name: c.GetName(), Version: c.GetVersion(), ControlCount: c.GetControlCount(), Found: true}
}

// describe renders r for text output. Name and version are uploader- or
// OSCAL-controlled, and a job created before CreateJob confirmed catalog
// ownership can name any ID, so all three are cleaned to one line. A
// catalog that was not read is named by its recorded label when the job has
// one, else by ID alone; a current name says it was not recorded with the
// job.
func (r catalogRef) describe() string {
	id := intanalyzer.CleanDisplay(r.ID, maxUntrustedDisplayBytes)
	var state string
	switch {
	case r.LookupError != "":
		state = "lookup failed: " + r.LookupError
	case !r.Found:
		state = "not found: deleted or purged, or not visible to your tenant"
	}
	if state != "" && !r.Snapshot {
		return id + " (" + state + ")"
	}
	version := intanalyzer.CleanDisplay(r.Version, maxUntrustedDisplayBytes)
	if version == "" {
		version = "not recorded"
	}
	name := intanalyzer.CleanDisplay(r.Name, maxUntrustedDisplayBytes)
	if state != "" {
		return fmt.Sprintf("%s (version %s, ID %s, %s)", name, version, id, state)
	}
	line := fmt.Sprintf("%s (version %s, ID %s, %d controls)", name, version, id, r.ControlCount)
	if !r.Snapshot {
		line += " (current name; not recorded when the job was created)"
	}
	return line
}

func (r catalogRef) toJSON() map[string]any {
	return map[string]any{
		"catalog_id":    r.ID,
		"name":          r.Name,
		"version":       r.Version,
		"control_count": r.ControlCount,
		"found":         r.Found,
		"lookup_error":  r.LookupError,
		"snapshot":      r.Snapshot,
	}
}

// toJSON returns the JSON form of jc: nil (null) for a job with no catalogs.
func (jc *jobCatalogs) toJSON() map[string]any {
	if jc == nil {
		return nil
	}
	return map[string]any{
		"source_catalog": jc.Source.toJSON(),
		"target_catalog": jc.Target.toJSON(),
		"direction":      jc.Direction,
	}
}
