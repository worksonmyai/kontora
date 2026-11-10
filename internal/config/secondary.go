package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type TicketSource struct {
	ID, Path string
}

// SecondarySources resolves and validates sources after environment and command-line overrides.
// LoadReader cannot check aliases because an override can change the primary directory.
// Missing or unreadable directories remain configured; callers must report their availability.
func (c *Config) SecondarySources() ([]TicketSource, error) {
	if len(c.SecondaryTicketsDirs) == 0 {
		return nil, nil
	}
	primary, err := sourceDirectoryIdentity(c.TicketsDir)
	if err != nil {
		return nil, fmt.Errorf("tickets_dir: %w", err)
	}
	primaryInfo, _ := os.Stat(primary)
	var identities []string
	var indices []int
	var sources []TicketSource
	for i, path := range c.SecondaryTicketsDirs {
		identity, err := sourceDirectoryIdentity(path)
		if err != nil {
			return nil, fmt.Errorf("secondary_tickets_dirs[%d]: %w", i, err)
		}
		info, _ := os.Stat(identity)
		if identity == primary || (info != nil && primaryInfo != nil && os.SameFile(info, primaryInfo)) {
			continue
		}
		for j, previous := range identities {
			previousInfo, _ := os.Stat(previous)
			if identity != previous && (info == nil || previousInfo == nil || !os.SameFile(info, previousInfo)) {
				continue
			}
			return nil, fmt.Errorf("secondary_tickets_dirs[%d]: %q aliases secondary_tickets_dirs[%d]", i, path, indices[j])
		}
		identities = append(identities, identity)
		indices = append(indices, i)
		hash := sha256.Sum256([]byte(identity))
		sources = append(sources, TicketSource{ID: fmt.Sprintf("secondary-%x", hash), Path: identity})
	}
	return sources, nil
}

func sourceDirectoryIdentity(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("directory path is empty")
	}
	absolute := ExpandTilde(path)
	if !filepath.IsAbs(absolute) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		// Preserve symlink/.. traversal until DirectoryIdentity resolves existing directories.
		absolute = wd + string(filepath.Separator) + absolute
	}
	return resolveSourceDirectory(absolute, 0), nil
}

func resolveSourceDirectory(path string, links int) string {
	if identity, err := DirectoryIdentity(path); err == nil {
		return identity
	}
	if links >= 40 {
		return filepath.Clean(path)
	}
	trimmed := strings.TrimRight(path, string(filepath.Separator))
	if target, err := os.Readlink(trimmed); err == nil {
		if !filepath.IsAbs(target) {
			parent, _ := filepath.Split(trimmed)
			target = parent + target
		}
		return resolveSourceDirectory(target, links+1)
	}
	// Resolve accessible ancestors without creating unavailable directories.
	parent, base := filepath.Split(trimmed)
	if base == "" {
		return filepath.Clean(path)
	}
	return filepath.Join(resolveSourceDirectory(parent, links), base)
}
