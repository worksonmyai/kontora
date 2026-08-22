package daemon

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

//go:embed pi_extension.js
var piExtensionJS string

// renderPiExtension adds the threshold, the run-kind gate and the waiting
// marker path to the extension. Annotation and Plannotator rework runs pass
// enabled=false. An empty waitFile leaves the question handlers unregistered,
// which is what a run the daemon does not poll wants.
//
// waitFile is marshalled rather than concatenated: a path is arbitrary bytes,
// and a quote or a backslash in one would otherwise end the JS string literal.
func renderPiExtension(threshold int, enabled bool, waitFile string) string {
	marker, err := json.Marshal(waitFile)
	if err != nil {
		// A string always marshals; fall back to no marker rather than to a
		// source file that cannot parse.
		marker = []byte(`""`)
	}
	s := piExtensionJS
	s = strings.ReplaceAll(s, "__CHECKPOINT_THRESHOLD__", fmt.Sprintf("%d", threshold))
	if enabled {
		s = strings.ReplaceAll(s, "__CHECKPOINT_ENABLED__", "true")
	} else {
		s = strings.ReplaceAll(s, "__CHECKPOINT_ENABLED__", "false")
	}
	s = strings.ReplaceAll(s, "__WAIT_MARKER_PATH__", string(marker))
	return s
}

// writePiExtension writes a temporary extension file. The caller removes it.
// A write or close error removes the partial file.
func writePiExtension(threshold int, enabled bool, waitFile string) (string, error) {
	rendered := renderPiExtension(threshold, enabled, waitFile)

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
