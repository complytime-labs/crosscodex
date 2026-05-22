package natsbus

import (
	"os"
	"path/filepath"
)

// xdgNATSStateDir returns the directory for NATS JetStream storage.
// Uses $XDG_STATE_HOME/crosscodex/nats/ per the XDG Base Directory
// Specification, falling back to $HOME/.local/state/crosscodex/nats/.
func xdgNATSStateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(base, "crosscodex", "nats")
}
