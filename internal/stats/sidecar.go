package stats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/worksonmyai/kontora/internal/logfmt"
)

// SidecarUsage is the four disjoint token categories written in a tape's
// metadata. CacheCreate is a cache write; unlike Usage.In, Input does not fold
// either cache category into itself.
type SidecarUsage struct {
	Input       int64
	Output      int64
	CacheCreate int64
	CacheRead   int64
}

// SidecarMetadata is the small, cost-relevant prefix of a tape. Usage is nil
// unless the prefix reaches events after all four integer categories and does
// not declare usage partial. An explicit four-category zero is measured data.
type SidecarMetadata struct {
	Model string
	Usage *SidecarUsage
}

// ReadSidecarMetadata reads only the metadata prefix of a tape sidecar and
// stops at the "events" key, which can hold up to logfmt.MaxTapeEvents entries
// and reach megabytes. A tape writes its metadata first, so this reads a few
// hundred bytes whatever the file's size, and anything malformed after the
// events key is never parsed.
func ReadSidecarMetadata(path string) (SidecarMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return SidecarMetadata{}, err
	}
	defer f.Close()

	dec := json.NewDecoder(bufio.NewReader(f))
	open, err := dec.Token()
	if err != nil {
		return SidecarMetadata{}, fmt.Errorf("read tape: %w", err)
	}
	if d, ok := open.(json.Delim); !ok || d != '{' {
		return SidecarMetadata{}, fmt.Errorf("tape is not a JSON object")
	}

	var (
		version     int
		model       string
		totals      json.RawMessage
		partial     []string
		readVersion bool
		readTotal   bool
		readEvents  bool
	)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return SidecarMetadata{}, fmt.Errorf("read tape key: %w", err)
		}
		name, _ := key.(string)
		if name == "events" {
			readEvents = true
			break
		}
		var target any
		switch name {
		case "version":
			target = &version
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
			return SidecarMetadata{}, fmt.Errorf("read tape field %q: %w", name, err)
		}
		readVersion = readVersion || name == "version"
		readTotal = readTotal || name == "totals"
	}

	metadata := SidecarMetadata{Model: model}
	if !readVersion || version != logfmt.TapeVersion {
		return metadata, nil
	}
	// The reader stops at events, so all required metadata must precede that
	// field. A different or incomplete object must not look like measured zero.
	if !readTotal || !readEvents || !logfmt.UsageComplete(partial) {
		return metadata, nil
	}
	metadata.Usage = decodeSidecarUsage(totals)
	return metadata, nil
}

// decodeSidecarUsage deliberately requires every category. Decoding directly
// into logfmt.Usage would make an omitted or null field indistinguishable from
// a genuine measured zero.
func decodeSidecarUsage(raw json.RawMessage) *SidecarUsage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	var usage SidecarUsage
	for name, target := range map[string]*int64{
		"input":        &usage.Input,
		"output":       &usage.Output,
		"cache_create": &usage.CacheCreate,
		"cache_read":   &usage.CacheRead,
	} {
		value, ok := fields[name]
		if !ok || string(value) == "null" || json.Unmarshal(value, target) != nil {
			return nil
		}
	}
	return &usage
}

// SidecarTotals preserves the Stats API's folded inbound count while the raw
// metadata remains available to cost calculations.
func SidecarTotals(path string) (string, *Usage, error) {
	metadata, err := ReadSidecarMetadata(path)
	if err != nil {
		return "", nil, err
	}
	if metadata.Usage == nil {
		return metadata.Model, nil, nil
	}
	u := metadata.Usage
	return metadata.Model, &Usage{
		In:          int(u.Input + u.CacheCreate + u.CacheRead),
		Out:         int(u.Output),
		CacheCreate: int(u.CacheCreate),
		CacheRead:   int(u.CacheRead),
	}, nil
}
