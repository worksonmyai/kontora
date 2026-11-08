package store

import (
	"errors"
	"os"
	"path/filepath"
)

// HasPendingTransfer excludes published files until transfer recovery completes.
func HasPendingTransfer(destination, id string) (bool, error) {
	destination, err := transferIdentity(destination)
	if err != nil {
		return false, err
	}
	root := filepath.Join(destination, transferDirectory)
	if err := checkTransferDirectory(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if pendingID := transferTokenID(entry.Name()); pendingID != "" && pendingID != id {
			continue
		}
		txn := filepath.Join(root, entry.Name())
		if _, err := os.Lstat(filepath.Join(txn, "record.json")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false, err
		}
		r, err := loadTransferRecord(txn, destination)
		if err != nil {
			return false, err
		}
		if r.ID == id {
			return true, nil
		}
	}
	return false, nil
}
