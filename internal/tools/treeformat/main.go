// Package main provides a tree formatter for Ginkgo JSON reports.
//
// It reads the JSON report produced by ginkgo --json-report and renders
// a nested tree showing Describe/Context/It hierarchy with pass/fail markers.
//
// Usage:
//
//	ginkgo run --json-report=report.json ./pkg/...
//	go run ./internal/tools/treeformat report.json
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// specReport mirrors the subset of Ginkgo's SpecReport we need.
type specReport struct {
	ContainerHierarchyTexts []string `json:"ContainerHierarchyTexts"`
	LeafNodeText            string   `json:"LeafNodeText"`
	LeafNodeType            string   `json:"LeafNodeType"`
	State                   string   `json:"State"`
	RunTime                 int64    `json:"RunTime"` // nanoseconds
}

// suiteReport mirrors the subset of Ginkgo's suite-level JSON.
type suiteReport struct {
	SuitePath        string       `json:"SuitePath"`
	SuiteDescription string       `json:"SuiteDescription"`
	SuiteSucceeded   bool         `json:"SuiteSucceeded"`
	RunTime          int64        `json:"RunTime"` // nanoseconds
	SpecReports      []specReport `json:"SpecReports"`
}

// treeNode represents one level in the Describe/Context/It hierarchy.
type treeNode struct {
	name     string
	children []*treeNode
	// leaf-only fields
	state   string
	runTime time.Duration
	isLeaf  bool
}

// insert adds a spec into the tree by walking its container hierarchy,
// creating intermediate nodes as needed, and placing the leaf (It) at the end.
func (n *treeNode) insert(containers []string, leafText string, state string, runTime time.Duration) {
	current := n
	for _, c := range containers {
		var found *treeNode
		for _, child := range current.children {
			if child.name == c && !child.isLeaf {
				found = child
				break
			}
		}
		if found == nil {
			found = &treeNode{name: c}
			current.children = append(current.children, found)
		}
		current = found
	}
	current.children = append(current.children, &treeNode{
		name:    leafText,
		state:   state,
		runTime: runTime,
		isLeaf:  true,
	})
}

// stateIcon returns a colored symbol for the spec state.
func stateIcon(state string) string {
	switch state {
	case "passed":
		return "\033[32m✓\033[0m"
	case "failed":
		return "\033[31m✗\033[0m"
	case "pending":
		return "\033[33m○\033[0m"
	case "skipped":
		return "\033[90m-\033[0m"
	default:
		return "?"
	}
}

// render prints the tree with box-drawing connectors.
func render(node *treeNode, prefix string, isLast bool, isRoot bool) {
	if !isRoot {
		connector := "├── "
		if isLast {
			connector = "└── "
		}
		if node.isLeaf {
			icon := stateIcon(node.state)
			dur := ""
			if node.runTime > 100*time.Millisecond {
				dur = fmt.Sprintf(" \033[90m(%s)\033[0m", node.runTime.Truncate(time.Millisecond))
			}
			fmt.Printf("%s%s%s %s%s\n", prefix, connector, icon, node.name, dur)
		} else {
			fmt.Printf("%s%s\033[1m%s\033[0m\n", prefix, connector, node.name)
		}
	}

	childPrefix := prefix
	if !isRoot {
		if isLast {
			childPrefix += "    "
		} else {
			childPrefix += "│   "
		}
	}

	for i, child := range node.children {
		last := i == len(node.children)-1
		render(child, childPrefix, last, false)
	}
}

// stats counts passed/failed/pending/skipped totals for a suite.
type stats struct {
	passed  int
	failed  int
	pending int
	skipped int
}

func (s stats) total() int {
	return s.passed + s.failed + s.pending + s.skipped
}

func countStats(specs []specReport) stats {
	var s stats
	for _, spec := range specs {
		switch spec.State {
		case "passed":
			s.passed++
		case "failed":
			s.failed++
		case "pending":
			s.pending++
		case "skipped":
			s.skipped++
		}
	}
	return s
}

func run() int {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: treeformat <report.json>\n")
		return 1
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading report: %v\n", err)
		return 1
	}

	var suites []suiteReport
	if err := json.Unmarshal(data, &suites); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing report: %v\n", err)
		return 1
	}

	// Sort suites by path for deterministic output.
	sort.Slice(suites, func(i, j int) bool {
		return suites[i].SuitePath < suites[j].SuitePath
	})

	hasFailure := false

	for i, suite := range suites {
		if i > 0 {
			fmt.Println()
		}

		// Show relative path from workspace root when possible.
		suiteName := suite.SuitePath
		if cwd, err := os.Getwd(); err == nil {
			if rel, err := filepath.Rel(cwd, suiteName); err == nil {
				suiteName = rel
			}
		}

		st := countStats(suite.SpecReports)
		dur := time.Duration(suite.RunTime).Truncate(time.Millisecond)

		// Suite header with summary.
		statusColor := "\033[32m" // green
		if !suite.SuiteSucceeded {
			statusColor = "\033[31m" // red
			hasFailure = true
		}
		fmt.Printf("%s%s\033[0m \033[90m(%d specs, %s)\033[0m\n", statusColor, suiteName, st.total(), dur)

		// Summary line.
		parts := []string{}
		if st.passed > 0 {
			parts = append(parts, fmt.Sprintf("\033[32m%d passed\033[0m", st.passed))
		}
		if st.failed > 0 {
			parts = append(parts, fmt.Sprintf("\033[31m%d failed\033[0m", st.failed))
		}
		if st.pending > 0 {
			parts = append(parts, fmt.Sprintf("\033[33m%d pending\033[0m", st.pending))
		}
		if st.skipped > 0 {
			parts = append(parts, fmt.Sprintf("\033[90m%d skipped\033[0m", st.skipped))
		}
		fmt.Printf("  %s\n", strings.Join(parts, " | "))

		// Build and render tree.
		root := &treeNode{name: suiteName}
		for _, spec := range suite.SpecReports {
			// Skip setup/teardown nodes, only show actual specs.
			if spec.LeafNodeType != "It" {
				continue
			}
			// Strip the top-level suite Describe (first container) since
			// we already show it as the suite header.
			containers := spec.ContainerHierarchyTexts
			if len(containers) > 0 {
				containers = containers[1:]
			}
			root.insert(
				containers,
				spec.LeafNodeText,
				spec.State,
				time.Duration(spec.RunTime),
			)
		}

		render(root, "", true, true)
	}

	if hasFailure {
		return 1
	}
	return 0
}

func main() {
	os.Exit(run())
}
