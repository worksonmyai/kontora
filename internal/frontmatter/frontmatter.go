package frontmatter

import (
	"errors"
	"strings"
)

// Split splits a markdown file into YAML frontmatter and body.
// It normalizes \r\n to \n. Only the first two --- delimiters are considered.
func Split(data string) (yaml string, body string, err error) {
	normalized := strings.ReplaceAll(data, "\r\n", "\n")

	if !strings.HasPrefix(normalized, "---\n") {
		return "", "", errors.New("no frontmatter found: file must start with ---")
	}

	rest := normalized[4:] // skip opening "---\n"
	yamlContent, after, found := strings.Cut(rest, "\n---\n")
	if !found {
		if strings.HasSuffix(rest, "\n---") {
			return rest[:len(rest)-4], "", nil
		}
		return "", "", errors.New("no closing --- found for frontmatter")
	}

	return yamlContent, after, nil
}
