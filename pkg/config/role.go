package config

import (
	"fmt"
	"net"
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

// RequireHealthAddr rejects a graph, pipeline or worker role whose
// health.addr is not host:port. Those roles bind their own plain-HTTP health
// listener on it (gateway and all serve /healthz on server.addr and ignore
// it), and an empty value binds an ephemeral port that no healthcheck can
// find. Validation matches graph.addr: net.SplitHostPort with a non-empty
// port.
//
// role must be canonical (see ResolveRole). Like RequireSharedNATS,
// crosscodexd calls this once on its effective role (the --role flag can
// differ from the configured role), before bootstrap.
func RequireHealthAddr(role, addr string) error {
	if role != RoleGraph && role != RolePipeline && role != RoleWorker {
		return nil
	}
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return nil
	}
	return fmt.Errorf("role %q needs health.addr %q to be host:port for its health listener: "+
		"set health.addr (CROSSCODEX_HEALTH_ADDR) to the address to bind, for example :9091: %w",
		role, addr, ErrInvalidConfig)
}
