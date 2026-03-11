package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

// DiskRepo reads and writes tickets as markdown files in a directory.
type DiskRepo struct {
	dir string
}

// NewDiskRepo creates a DiskRepo for the given tickets directory.
// The path is expanded (tilde resolved) on construction.
func NewDiskRepo(ticketsDir string) *DiskRepo {
	return &DiskRepo{dir: config.ExpandTilde(ticketsDir)}
}

func (r *DiskRepo) Resolve(idOrPrefix string) (string, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return "", fmt.Errorf("reading tickets dir: %w", err)
	}

	var prefixMatch string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		if name == idOrPrefix {
			return idOrPrefix, nil
		}
		if prefixMatch == "" && strings.HasPrefix(name, idOrPrefix) {
			prefixMatch = name
		}
	}

	if prefixMatch != "" {
		return prefixMatch, nil
	}
	return "", fmt.Errorf("ticket %q not found", idOrPrefix)
}

func (r *DiskRepo) Get(id string) (*app.StoredTicket, error) {
	path := filepath.Join(r.dir, id+".md")
	t, err := ticket.ParseFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading ticket %s: %w", id, err)
	}
	return &app.StoredTicket{Ticket: t, FilePath: path}, nil
}

func (r *DiskRepo) List() ([]*app.StoredTicket, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading tickets dir: %w", err)
	}

	var result []*app.StoredTicket
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(r.dir, entry.Name())
		t, err := ticket.ParseFile(path)
		if err != nil {
			continue
		}
		result = append(result, &app.StoredTicket{Ticket: t, FilePath: path})
	}
	return result, nil
}

func (r *DiskRepo) Save(st *app.StoredTicket) error {
	out, err := st.Ticket.Marshal()
	if err != nil {
		return fmt.Errorf("marshalling ticket: %w", err)
	}
	return os.WriteFile(st.FilePath, out, 0o644)
}
