package stats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/worksonmyai/kontora/internal/logfmt"
)

// SidecarTotals reads the model and token counts of a tape sidecar and stops
// at the "events" key, which can hold up to logfmt.MaxTapeEvents entries and
// reach megabytes. A tape writes its metadata first, so this reads a few hundred
// bytes whatever the file's size, and anything malformed after the events key
// is never parsed.
//
// The returned usage is nil when the tape declares usage partial: such a tape
// still writes a total, but one missing whatever its unreadable records spent,
// so it is not what the run cost. In folds the three inbound categories
// together, so it is every token the model was fed; CacheCreate and CacheRead
// name two of them again.
func SidecarTotals(path string) (string, *Usage, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(bufio.NewReader(f))
	open, err := dec.Token()
	if err != nil {
		return "", nil, fmt.Errorf("read tape: %w", err)
	}
	if d, ok := open.(json.Delim); !ok || d != '{' {
		return "", nil, fmt.Errorf("tape is not a JSON object")
	}

	var (
		model     string
		totals    logfmt.Usage
		partial   []string
		readTotal bool
	)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return "", nil, fmt.Errorf("read tape key: %w", err)
		}
		name, _ := key.(string)
		if name == "events" {
			break
		}
		var target any
		switch name {
		case "model":
			target = &model
		case "totals":
			target = &totals
		case "partial":
			target = &partial
		default:
			target = new(json.RawMessage)
		}
		if err := dec.Decode(target); err != nil {
			return "", nil, fmt.Errorf("read tape field %q: %w", name, err)
		}
		readTotal = readTotal || name == "totals"
	}

	// Nothing in logfmt promises that events is written last. A tape that puts
	// it first leaves totals unread, and reporting that zero as a measured count
	// would blank the token figures without an error.
	if !readTotal || !logfmt.UsageComplete(partial) {
		return model, nil, nil
	}
	return model, &Usage{
		In:          totals.Input + totals.CacheCreate + totals.CacheRead,
		Out:         totals.Output,
		CacheCreate: totals.CacheCreate,
		CacheRead:   totals.CacheRead,
	}, nil
}
