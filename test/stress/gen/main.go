package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	controlsFlag := flag.Int("controls", 10, "number of controls to generate")
	outFlag := flag.String("out", "", "output file path (required)")
	flag.Parse()

	if *outFlag == "" {
		fmt.Fprintln(os.Stderr, "error: -out flag is required")
		os.Exit(1)
	}

	if *controlsFlag < 1 {
		fmt.Fprintln(os.Stderr, "error: -controls must be >= 1")
		os.Exit(1)
	}

	catalog := generateCatalog(*controlsFlag)

	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to marshal JSON: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(*outFlag, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to write file: %v\n", err)
		os.Exit(1)
	}

	// Print the control count to stdout for Venom to capture
	fmt.Println(*controlsFlag)
}

func generateCatalog(numControls int) map[string]any {
	controls := make([]map[string]any, numControls)
	for i := 0; i < numControls; i++ {
		controlID := fmt.Sprintf("stress-%d", i+1)
		controls[i] = map[string]any{
			"id":    controlID,
			"title": fmt.Sprintf("Stress Test Control %d", i+1),
			"parts": []map[string]any{
				{
					"id":    fmt.Sprintf("%s_smt", controlID),
					"name":  "statement",
					"prose": fmt.Sprintf("This is stress test control %d for round-trip integrity verification.", i+1),
				},
			},
		}
	}

	return map[string]any{
		"catalog": map[string]any{
			"uuid": "12345678-1234-4234-8234-123456789000",
			"metadata": map[string]any{
				"title":         "Stress Test Catalog",
				"version":       "1.0",
				"oscal-version": "1.1.3",
				"last-modified": time.Now().UTC().Format(time.RFC3339),
			},
			"groups": []map[string]any{
				{
					"id":       "stress-test-group",
					"title":    "Stress Test Group",
					"controls": controls,
				},
			},
		},
	}
}
