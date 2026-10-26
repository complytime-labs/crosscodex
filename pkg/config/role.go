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

// RequireSharedNATS rejects a split role (any role but all) whose NATS
// connection would run embedded. Split roles reach each other only over
// NATS, and an empty natsURL makes pkg/natsbus start a private embedded
// server inside each process, so every split role would boot healthy while
// nothing crossed between them. Only role all, a single process, can use the
// embedded server.
//
// role must be canonical (see ResolveRole); an empty or alias role is
// treated as a split role and rejected.
//
// The empty-string test mirrors pkg/natsbus isEmbeddedMode; it is repeated
// rather than shared because pkg/natsbus imports pkg/config.
//
// crosscodexd calls this once, on its effective role (--role flag, else the
// configured role), before bootstrap. Config loading does not call it: the
// crosscodex CLI, crosscodexd healthcheck and admin subcommands load the
// same configuration without starting a role.
func RequireSharedNATS(role, natsURL string) error {
	if role == RoleAll || natsURL != "" {
		return nil
	}
	return fmt.Errorf("role %q requires nats.url to point at the NATS server shared by every crosscodexd role; "+
		"an empty nats.url starts a private embedded server that no other role can reach. "+
		"Set nats.url (CROSSCODEX_NATS_URL) to the shared server, "+
		"or set the role to all (--role all or CROSSCODEX_ROLE=all) for a single-process deployment: %w",
		role, ErrInvalidConfig)
}
