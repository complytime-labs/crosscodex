package config

import (
	"os"
	"path/filepath"
	"strings"
)

const appName = "crosscodex"

type resolvedPaths struct {
	systemConfig    string
	systemDropInDir string
	userConfig      string
	userDropInDir   string
}

func xdgConfigHome() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(os.Getenv("HOME"), ".config")
}

func userConfigDir() string {
	return filepath.Join(xdgConfigHome(), appName)
}

func profilePath(name string) string {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return ""
	}
	clean := filepath.Base(filepath.Clean(name))
	if clean == "." || clean == ".." {
		return ""
	}
	return filepath.Join(userConfigDir(), "profiles", clean+".yaml")
}

func configPaths() resolvedPaths {
	userDir := userConfigDir()
	return resolvedPaths{
		systemConfig:    filepath.Join("/etc", appName, "config.yaml"),
		systemDropInDir: filepath.Join("/etc", appName, "conf.d"),
		userConfig:      filepath.Join(userDir, "config.yaml"),
		userDropInDir:   filepath.Join(userDir, "conf.d"),
	}
}
