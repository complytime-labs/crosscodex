package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// spool copies r into a new file in dir (os.TempDir when dir is ""),
// hashing as it goes, and returns the file rewound to its start. S3 uploads
// need a seekable body, and restore must check a blob's hash before writing
// it anywhere. The caller disposes of the file with closeAndRemove.
func spool(dir string, r io.Reader) (f *os.File, sum string, size int64, err error) {
	f, err = os.CreateTemp(dir, "crosscodex-backup-*")
	if err != nil {
		return nil, "", 0, fmt.Errorf("create spool file: %w", err)
	}
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		return nil, "", 0, errors.Join(fmt.Errorf("spool: %w", err), closeAndRemove(f))
	}
	return f, hex.EncodeToString(h.Sum(nil)), size, nil
}

func closeAndRemove(f *os.File) error {
	return errors.Join(f.Close(), os.Remove(f.Name()))
}

// hashReader returns the sha256 hex digest and length of everything r yields.
func hashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
