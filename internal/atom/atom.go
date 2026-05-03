package atom

import (
	"os"
	"path/filepath"
)

// atom.go — atomic file write (write to temp + rename) to prevent corruption.

// WriteFile writes data to a file atomically. The file is written to a
// temporary file in the same directory, then renamed over the target.
// This prevents partial writes if the process crashes mid-write.
func WriteFile(name string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(name)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// Write data
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}

	// Sync to disk
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	// Atomic rename
	return os.Rename(tmpName, name)
}
