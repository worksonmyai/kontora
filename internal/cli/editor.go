package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// openEditor launches the given editor with the file at path.
// Falls back to $EDITOR, then "vi" if editor is empty.
// Supports editors with arguments (e.g., "nvim -u init.lua").
func openEditor(editor, path string) error {
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}

	parts := strings.Fields(editor)
	if len(parts) == 0 {
		parts = []string{"vi"}
	}

	bin := parts[0]
	args := make([]string, 0, len(parts))
	args = append(args, parts[1:]...)
	args = append(args, path)
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor: %w", err)
	}
	return nil
}
