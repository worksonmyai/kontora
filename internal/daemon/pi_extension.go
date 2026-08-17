package daemon

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
)

//go:embed pi_extension.js
var piExtensionJS string

// renderPiExtension adds the threshold and run-kind gate to the extension.
// Annotation and Plannotator rework runs pass enabled=false.
func renderPiExtension(threshold int, enabled bool) string {
	s := piExtensionJS
	s = strings.ReplaceAll(s, "__CHECKPOINT_THRESHOLD__", fmt.Sprintf("%d", threshold))
	if enabled {
		s = strings.ReplaceAll(s, "__CHECKPOINT_ENABLED__", "true")
	} else {
		s = strings.ReplaceAll(s, "__CHECKPOINT_ENABLED__", "false")
	}
	return s
}

// writePiExtension writes a temporary extension file. The caller removes it.
// A write or close error removes the partial file.
func writePiExtension(threshold int, enabled bool) (string, error) {
	rendered := renderPiExtension(threshold, enabled)

	f, err := os.CreateTemp("", "kontora-pi-ext-*.js")
	if err != nil {
		return "", err
	}

	if _, err := f.WriteString(rendered); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}

	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	return f.Name(), nil
}
