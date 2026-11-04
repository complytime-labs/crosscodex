package testcerts

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// EnsureOptions configures Ensure.
type EnsureOptions struct {
	// Verify is what an existing set must satisfy to be kept. Ensure always
	// writes .fingerprint, so it also requires one regardless of this value.
	Verify VerifyOptions
	// Generate mints a new PKI when the existing set fails verification.
	Generate func() (*PKI, error)
	// WriteExtra, if set, writes caller-owned files (checked by
	// Verify.VerifyExtra) into the staging directory. Ensure fsyncs them.
	WriteExtra func(dir string) error
}

// Ensure makes dir hold a cert set that passes VerifyDirWith(dir, opts.Verify),
// generating one only when needed, and reports whether it generated.
//
// It is safe to run concurrently from many processes: it takes an exclusive
// flock on a lock file next to dir (".<base>.lock" in dir's parent, never
// removed, so every run locks the same inode), re-verifies under the lock so
// a run that waited reuses the set the winner just wrote, and otherwise
// builds the complete set in a staging directory in the same parent, fsyncs
// it, verifies it, and renames it into place. Readers therefore see the old
// set, the new set, or (for the instant between the two renames that replace
// an existing dir) no dir, but never a mix. Readers holding open files keep
// reading the old set's contents. dir must be dedicated to the set: anything
// else in it is discarded on regeneration.
func Ensure(dir string, opts EnsureOptions) (bool, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return false, fmt.Errorf("resolve cert dir: %w", err)
	}
	parent, base := filepath.Dir(dir), filepath.Base(dir)
	opts.Verify.RequireFingerprint = true

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, fmt.Errorf("create parent of cert dir: %w", err)
	}
	unlock, err := lockFile(filepath.Join(parent, "."+base+".lock"))
	if err != nil {
		return false, err
	}
	defer unlock()

	if err := removeLeftovers(parent, base); err != nil {
		return false, err
	}
	if VerifyDirWith(dir, opts.Verify) == nil {
		return false, nil
	}

	staging, err := os.MkdirTemp(parent, "."+base+".new-")
	if err != nil {
		return false, fmt.Errorf("create staging dir for %s: %w", dir, err)
	}
	if err := populate(staging, opts); err != nil {
		return false, errors.Join(err, os.RemoveAll(staging))
	}
	if err := swapInto(staging, dir); err != nil {
		return false, errors.Join(err, os.RemoveAll(staging))
	}
	return true, nil
}

// lockFile takes an exclusive flock on path, creating it if needed, and
// returns the release function. The kernel drops the lock if the process dies.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open cert lock %s: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("lock %s: %w", path, err), f.Close())
	}
	// Closing the descriptor releases the flock.
	return func() { _ = f.Close() }, nil
}

// removeLeftovers deletes staging and set-aside dirs from a run that died
// mid-Ensure. Only called under the lock, so none belongs to a live run.
func removeLeftovers(parent, base string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return fmt.Errorf("scan %s for leftover cert dirs: %w", parent, err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "."+base+".new-") || strings.HasPrefix(name, "."+base+".old-") {
			if err := os.RemoveAll(filepath.Join(parent, name)); err != nil {
				return fmt.Errorf("remove leftover cert dir %s: %w", name, err)
			}
		}
	}
	return nil
}

// populate generates and writes the full set into staging, makes it durable,
// and checks it meets the requirements before it may replace anything.
func populate(staging string, opts EnsureOptions) error {
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("chmod staging dir: %w", err)
	}
	p, err := opts.Generate()
	if err != nil {
		return fmt.Errorf("generate PKI: %w", err)
	}
	if err := p.WriteToDir(staging); err != nil {
		return err
	}
	if err := WriteFingerprint(staging); err != nil {
		return fmt.Errorf("write fingerprint: %w", err)
	}
	if opts.WriteExtra != nil {
		if err := opts.WriteExtra(staging); err != nil {
			return err
		}
	}
	if err := syncDir(staging); err != nil {
		return err
	}
	if err := verifyDir(staging, opts.Verify); err != nil {
		return fmt.Errorf("newly generated cert set fails its own requirements: %w; fix the generator options so they produce what verification requires", err)
	}
	return nil
}

// syncDir fsyncs every regular file in dir, then dir itself.
func syncDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if err := syncPath(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return syncPath(dir)
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", path, err), f.Close())
	}
	return f.Close()
}

// swapInto renames staging to dir. An existing dir is first renamed aside
// and removed after the new set is in place; if the second rename fails the
// old set is restored.
func swapInto(staging, dir string) error {
	parent := filepath.Dir(dir)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(staging, dir); err != nil {
			return fmt.Errorf("install cert dir %s: %w", dir, err)
		}
		return syncPath(parent)
	} else if err != nil {
		return fmt.Errorf("stat cert dir %s: %w", dir, err)
	}

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("name set-aside cert dir: %w", err)
	}
	old := filepath.Join(parent, "."+filepath.Base(dir)+".old-"+hex.EncodeToString(suffix))
	if err := os.Rename(dir, old); err != nil {
		return fmt.Errorf("move old cert dir %s aside: %w", dir, err)
	}
	if err := os.Rename(staging, dir); err != nil {
		return errors.Join(fmt.Errorf("install cert dir %s: %w", dir, err), os.Rename(old, dir))
	}
	if err := syncPath(parent); err != nil {
		return err
	}
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("remove old cert dir %s: %w", old, err)
	}
	return nil
}
