package config

import (
	"fmt"
	"strings"
)

// Canonical daemon roles crosscodexd can actually start. See DaemonConfig
// and cmd/crosscodexd for how each is wired.
const (
	RoleAll      = "all"
	RoleGateway  = "gateway"
	RolePipeline = "pipeline"
	RoleWorker   = "worker"
	RoleGraph    = "graph"
)

// CanonicalRoles lists every role crosscodexd can actually start.
var CanonicalRoles = []string{RoleAll, RoleGateway, RolePipeline, RoleWorker, RoleGraph}

// roleAliases maps deployment-facing role names to the runtime role that
// currently implements them. Analysis and synthesis execute synchronously
// inside pipeline.Service, so they resolve to the pipeline role --
// whichever process actually runs pipeline.Service (RolePipeline
// standalone, or RoleAll) is what executes them.
var roleAliases = map[string]string{
	"analysis":  RolePipeline,
	"synthesis": RolePipeline,
}

// ResolveRole validates a requested role name and returns the canonical
// role that implements it, resolving the analysis/synthesis aliases to
// pipeline. Returns an actionable error listing every accepted value when
// raw matches neither a canonical role nor a known alias.
func ResolveRole(raw string) (string, error) {
	for _, canonical := range CanonicalRoles {
		if raw == canonical {
			return canonical, nil
		}
	}
	if canonical, ok := roleAliases[raw]; ok {
		return canonical, nil
	}
	return "", fmt.Errorf("role %q must be one of %s (or an alias: analysis, synthesis): %w",
		raw, strings.Join(CanonicalRoles, ", "), ErrInvalidConfig)
}
