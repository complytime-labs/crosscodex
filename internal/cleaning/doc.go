// Package cleaning implements the Connect EdgeCleaningService handler. It
// authorizes each RPC by role, scopes it to the caller's tenant, converts
// requests and results between the proto and pkg/cleaning, and delegates the
// work to a cleaning engine (pkg/cleaning.Engine).
//
// # Authorization
//
// RunCleaning needs authn.RoleAdmin. ListFindings and GetFinding need
// authn.RoleReviewer or authn.RoleAdmin. DecideFinding needs
// authn.RoleReviewer: an admin alone cannot decide, so applying a pass and
// deciding a finding stay separate duties. The decider is the authenticated
// subject; no request field names it.
//
// # Errors
//
// The handler is mounted on the gateway server behind its error policy
// (publicErrorInterceptor), which passes caller-actionable codes through and
// withholds everything else as Internal (CWE-209). Engine sentinels the
// caller can act on map to InvalidArgument, NotFound, FailedPrecondition or
// PermissionDenied with the engine's own message, which never names another
// tenant. When such an error also wraps something other than an engine
// sentinel (a driver error, say), only the sentinel's own text is sent.
// Every other engine error is returned raw, so the policy withholds it.
package cleaning
