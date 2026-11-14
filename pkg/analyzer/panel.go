package analyzer

// Panel is the set of model aliases whose votes an analyzer's consensus
// counts, and whether the operator allows them to share one upstream model.
type Panel struct {
	// Aliases are the panel's model aliases, as the gateway receives them.
	Aliases []string
	// AllowShared is the analyzer's allow_shared_panel_models setting: when
	// true, aliases that resolve to the same (or an unknown) upstream model
	// are accepted, and the job's provenance says so.
	AllowShared bool
}

// Panelled is the optional interface an analyzer implements when its result
// is a vote across several model aliases. The pipeline checks, before any
// analysis call, that the aliases of Panel do not share an upstream model.
// Implementations must be safe for concurrent use.
type Panelled interface {
	Panel() Panel
}

// AsPanelled returns ra's analyzer as a Panelled, and false when it is not
// a panel. It sees through the registry's wrapper.
func AsPanelled(ra RegisteredAnalyzer) (Panelled, bool) {
	if u, ok := ra.(unwrapper); ok {
		p, ok := u.unwrap().(Panelled)
		return p, ok
	}
	p, ok := ra.(Panelled)
	return p, ok
}
