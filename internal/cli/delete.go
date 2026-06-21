package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
)

// Delete removes a ticket markdown file from the tickets directory. It resolves
// the ID (prefix allowed), then guards that the target is a .md file directly
// inside the tickets dir before removing it, mirroring the daemon's delete-path
// guard so a crafted ID can't escape the directory.
func Delete(ticketsDir, id string) error {
	ticketsDir = config.ExpandTilde(ticketsDir)
	resolved, err := resolveTaskID(ticketsDir, id)
	if err != nil {
		return err
	}

	path := filepath.Join(ticketsDir, resolved+".md")

	dirAbs, err := filepath.Abs(ticketsDir)
	if err != nil {
		return fmt.Errorf("resolve tickets dir: %w", err)
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve ticket path: %w", err)
	}
	if filepath.Dir(pathAbs) != dirAbs {
		return fmt.Errorf("refusing to delete file outside tickets dir: %s", pathAbs)
	}
	if !strings.EqualFold(filepath.Ext(pathAbs), ".md") {
		return fmt.Errorf("refusing to delete non-markdown ticket file: %s", pathAbs)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing ticket file: %w", err)
	}
	return nil
}
