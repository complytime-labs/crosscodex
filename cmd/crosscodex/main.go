package main

import (
	"fmt"
	"os"

	"github.com/complytime-labs/crosscodex/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		info := version.GetInfo()
		fmt.Printf("crosscodex %s (commit: %s, built: %s, go: %s, %s/%s)\n",
			info.Version, info.GitCommit, info.BuildDate,
			info.GoVersion, info.OS, info.Arch)
		return
	}

	fmt.Println("CrossCodex CLI - not yet implemented")
	os.Exit(0)
}
