package main

import (
	"fmt"
	"os"

	"github.com/complytime-labs/crosscodex/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("crosscodex %s (commit: %s, built: %s)\n",
			version.Version, version.GitCommit, version.BuildDate)
		return
	}

	fmt.Println("CrossCodex CLI - not yet implemented")
	os.Exit(0)
}
