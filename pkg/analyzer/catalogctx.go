package analyzer

import "context"

type catalogIDKey struct{}

// WithCatalogID returns a new context carrying the given catalog ID.
func WithCatalogID(ctx context.Context, catalogID string) context.Context {
	return context.WithValue(ctx, catalogIDKey{}, catalogID)
}

// CatalogIDFromContext extracts the catalog ID from the context.
// Returns ok=false if no catalog ID is present.
func CatalogIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(catalogIDKey{}).(string)
	return v, ok
}

type controlCatalogsKey struct{}

// WithControlCatalogs returns a new context carrying the catalog ID of each
// control in a per-control analyzer run, keyed by control ID. A job that
// maps one catalog onto another fans out the controls of both catalogs in
// one run, so the run-wide CatalogIDFromContext names only the source; an
// analyzer that persists per-control data under a catalog reads each
// control's own catalog here. The map is not copied: the caller must not
// modify it afterwards.
func WithControlCatalogs(ctx context.Context, catalogs map[string]string) context.Context {
	return context.WithValue(ctx, controlCatalogsKey{}, catalogs)
}

// ControlCatalogFromContext returns the catalog ID WithControlCatalogs
// recorded for controlID. ok is false when the context has no record for
// the control, or the recorded catalog ID is empty.
func ControlCatalogFromContext(ctx context.Context, controlID string) (string, bool) {
	catalogs, _ := ctx.Value(controlCatalogsKey{}).(map[string]string)
	catalogID := catalogs[controlID]
	return catalogID, catalogID != ""
}

// HasControlCatalogs reports whether WithControlCatalogs recorded a
// per-control catalog map in ctx, even an empty one. A run that records the
// map names every control's catalog, so a control absent from it is an
// error, not a cue to fall back to the run-wide CatalogIDFromContext.
func HasControlCatalogs(ctx context.Context) bool {
	_, ok := ctx.Value(controlCatalogsKey{}).(map[string]string)
	return ok
}
