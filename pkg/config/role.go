package config

import (
	"fmt"
	"strings"
)

// Canonical daemon roles crosscodexd can actually start. See DaemonConfig
// and cmd/crosscodexd for how each is wired.
const (
	RoleAll     = "all"
	RoleGateway = "gateway"
	RoleWorker  = "worker"
	RoleGraph   = "graph"
)

// CanonicalRoles lists every role crosscodexd can actually start.
var CanonicalRoles = []string{RoleAll, RoleGateway, RoleWorker, RoleGraph}

// roleAliases maps deployment-facing role names to the runtime role that
// currently implements them. Pipeline job creation only happens in the
// process that embeds pipeline.Service and receives the CreateJob RPC --
// today that's always the gateway role (internal/gateway.PipelineBackend is
// wired directly to pipeline.Service in-process; there is no standalone
// network-addressable pipeline service). Analysis and synthesis execute
// synchronously inside pipeline.Service, so they resolve the same way.
var roleAliases = map[string]string{
	"pipeline":  RoleGateway,
	"analysis":  RoleGateway,
	"synthesis": RoleGateway,
}

// ResolveRole validates a requested role name and returns the canonical role
// that implements it, resolving the pipeline/analysis/synthesis aliases to
// gateway. Returns an actionable error listing every accepted value when raw
// matches neither a canonical role nor a known alias.
func ResolveRole(raw string) (string, error) {
	for _, canonical := range CanonicalRoles {
		if raw == canonical {
			return canonical, nil
		}
	}
	if canonical, ok := roleAliases[raw]; ok {
		return canonical, nil
	}
	return "", fmt.Errorf("role %q must be one of %s (or an alias: pipeline, analysis, synthesis): %w",
		raw, strings.Join(CanonicalRoles, ", "), ErrInvalidConfig)
}
