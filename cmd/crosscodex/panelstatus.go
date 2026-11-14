package main

import (
	"fmt"
	"io"
	"strings"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
)

// panelModelJSON is one alias of a panel and the model the gateway reported.
type panelModelJSON struct {
	Alias         string `json:"alias"`
	ObservedModel string `json:"observed_model"`
	Source        string `json:"source"`
	DeploymentID  string `json:"deployment_id"`
	Fault         string `json:"fault,omitempty"`
}

// panelJSON is one panel stage's provenance in run status --json.
type panelJSON struct {
	Stage        string           `json:"stage"`
	Independence string           `json:"independence"`
	AllowShared  bool             `json:"allow_shared_panel_models"`
	Models       []panelModelJSON `json:"models"`
}

// panelsOf returns the stages of job that carry panel provenance, in stage order.
func panelsOf(stages []*pb.JobStage) []panelJSON {
	out := []panelJSON{}
	for _, st := range stages {
		p := st.GetPanel()
		if p == nil {
			continue
		}
		models := make([]panelModelJSON, 0, len(p.GetModels()))
		for _, m := range p.GetModels() {
			models = append(models, panelModelJSON{Alias: m.GetAlias(), ObservedModel: m.GetObservedModel(),
				Source: m.GetSource(), DeploymentID: m.GetDeploymentId(), Fault: m.GetFault()})
		}
		out = append(out, panelJSON{Stage: st.GetStageName(), Independence: p.GetIndependence(),
			AllowShared: p.GetAllowShared(), Models: models})
	}
	return out
}

// writePanelLines prints, per panel stage, the model each alias reported and
// whether the shared-model allowance was in effect.
func writePanelLines(w io.Writer, panels []panelJSON) {
	if len(panels) == 0 {
		return
	}
	fmt.Fprintf(w, "Panels:\n")
	for _, p := range panels {
		parts := make([]string, len(p.Models))
		for i, m := range p.Models {
			observed := intanalyzer.CleanDisplay(m.ObservedModel, maxUntrustedDisplayBytes)
			if observed == "" {
				observed = "unknown"
			}
			parts[i] = intanalyzer.CleanDisplay(m.Alias, maxUntrustedDisplayBytes) + " → " + observed
			if m.Source != "" {
				note := m.Source
				if m.Fault != "" {
					note += ": " + m.Fault
				}
				parts[i] += " (" + intanalyzer.CleanDisplay(note, maxUntrustedDisplayBytes) + ")"
			}
		}
		allowance := "off"
		if p.AllowShared {
			allowance = "on"
		}
		line := fmt.Sprintf("  %s  %s; shared-model allowance %s", intanalyzer.CleanDisplay(p.Stage, maxUntrustedDisplayBytes),
			intanalyzer.CleanDisplay(p.Independence, maxUntrustedDisplayBytes), allowance)
		if len(parts) > 0 {
			line += "; " + strings.Join(parts, ", ")
		}
		fmt.Fprintln(w, line)
	}
}
