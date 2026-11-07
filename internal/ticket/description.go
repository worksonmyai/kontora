package ticket

import (
	"fmt"
	"strings"
)

// SplitDescription keeps the Notes heading and all later sections in the raw tail.
func SplitDescription(body string) (description, tail string) {
	lines := strings.Split(body, "\n")
	start, _ := notesSection(lines)
	if start < 0 {
		return body, ""
	}
	offset := 0
	for _, line := range lines[:start-1] {
		offset += len(line) + 1
	}
	return body[:offset], body[offset:]
}

func ValidateDescription(description string) error {
	if _, tail := SplitDescription(description); tail != "" {
		return fmt.Errorf("description must not contain a ## Notes heading")
	}
	return nil
}

// SetDescription replaces the description without changing the raw tail.
func (t *Ticket) SetDescription(description string) error {
	if err := ValidateDescription(description); err != nil {
		return err
	}
	_, tail := SplitDescription(t.Body)
	// Keep the heading on its own line. Existing separator bytes belong to the prefix.
	if tail != "" && description != "" && !strings.HasSuffix(description, "\n") {
		description += "\n"
	}
	t.SetBody(description + tail)
	return nil
}
