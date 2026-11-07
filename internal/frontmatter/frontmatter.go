package frontmatter

import (
	"errors"
	"strings"
)

// Split splits a markdown file into YAML frontmatter and body.
// It normalizes YAML line endings but preserves body bytes. Only the first two --- delimiters are considered.
func Split(data string) (yaml string, body string, err error) {
	rest, found := strings.CutPrefix(data, "---\n")
	if !found {
		rest, found = strings.CutPrefix(data, "---\r\n")
	}
	if !found {
		return "", "", errors.New("no frontmatter found: file must start with ---")
	}

	end, delimiterLen := strings.Index(rest, "\n---\n"), len("\n---\n")
	if crlfEnd := strings.Index(rest, "\n---\r\n"); crlfEnd >= 0 && (end < 0 || crlfEnd < end) {
		end, delimiterLen = crlfEnd, len("\n---\r\n")
	}
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			end, delimiterLen = len(rest)-len("\n---"), len("\n---")
		} else {
			return "", "", errors.New("no closing --- found for frontmatter")
		}
	}

	yamlContent := strings.TrimSuffix(rest[:end], "\r")
	return strings.ReplaceAll(yamlContent, "\r\n", "\n"), rest[end+delimiterLen:], nil
}
