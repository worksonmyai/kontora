package cli

import "strings"

// ParseNotifyList returns a non-nil empty slice for an explicit empty flag.
// A nil slice is reserved for an absent flag.
func ParseNotifyList(value string) []string {
	items := []string{}
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}
