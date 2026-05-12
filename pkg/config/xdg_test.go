package config

import (
	"path/filepath"
	"testing"
)

func TestXDGConfigHome_WithEnvSet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")
	t.Setenv("HOME", "/home/testuser")

	got := xdgConfigHome()
	want := "/custom/config"
	if got != want {
		t.Errorf("xdgConfigHome() = %q, want %q", got, want)
	}
}

func TestXDGConfigHome_FallbackToHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/testuser")

	got := xdgConfigHome()
	want := "/home/testuser/.config"
	if got != want {
		t.Errorf("xdgConfigHome() = %q, want %q", got, want)
	}
}

func TestUserConfigDir_WithXDGSet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")

	got := userConfigDir()
	want := "/custom/config/crosscodex"
	if got != want {
		t.Errorf("userConfigDir() = %q, want %q", got, want)
	}
}

func TestUserConfigDir_FallbackToHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/testuser")

	got := userConfigDir()
	want := filepath.Join("/home/testuser/.config", "crosscodex")
	if got != want {
		t.Errorf("userConfigDir() = %q, want %q", got, want)
	}
}

func TestConfigPaths_SystemPaths(t *testing.T) {
	paths := configPaths()
	if paths.systemConfig != "/etc/crosscodex/config.yaml" {
		t.Errorf("systemConfig = %q, want %q", paths.systemConfig, "/etc/crosscodex/config.yaml")
	}
	if paths.systemDropInDir != "/etc/crosscodex/conf.d" {
		t.Errorf("systemDropInDir = %q, want %q", paths.systemDropInDir, "/etc/crosscodex/conf.d")
	}
}

func TestConfigPaths_UserPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")

	paths := configPaths()
	if paths.userConfig != "/custom/config/crosscodex/config.yaml" {
		t.Errorf("userConfig = %q, want %q", paths.userConfig, "/custom/config/crosscodex/config.yaml")
	}
	if paths.userDropInDir != "/custom/config/crosscodex/conf.d" {
		t.Errorf("userDropInDir = %q, want %q", paths.userDropInDir, "/custom/config/crosscodex/conf.d")
	}
}

func TestProfilePath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")

	got := profilePath("local")
	want := "/custom/config/crosscodex/profiles/local.yaml"
	if got != want {
		t.Errorf("profilePath(\"local\") = %q, want %q", got, want)
	}
}

func TestProfilePath_RejectsTraversal(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"dotdot slash", "../../../etc/passwd"},
		{"slash prefix", "/etc/passwd"},
		{"backslash", "..\\..\\etc\\passwd"},
		{"dotdot only", ".."},
		{"dot only", "."},
		{"embedded slash", "foo/bar"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := profilePath(tt.input)
			if got != "" {
				t.Errorf("profilePath(%q) = %q, want empty (rejected)", tt.input, got)
			}
		})
	}
}
