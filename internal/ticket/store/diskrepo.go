package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

// Resolve maps an ID or unique prefix to a ticket ID. It matches against the
// IDs of the parsed canonical files rather than against filenames, so a sync
// conflict copy ("<id> 2.md") never answers for the ticket it duplicates and a
// file whose name disagrees with its frontmatter id resolves to neither.
func (r *DiskRepo) Resolve(idOrPrefix string) (string, error) {
	stored, err := r.List()
	if err != nil {
		return "", err
	}

	var prefixMatches []string
	for _, st := range stored {
		id := st.Ticket.ID
		if id == idOrPrefix {
			return id, nil
		}
		if strings.HasPrefix(id, idOrPrefix) {
			prefixMatches = append(prefixMatches, id)
		}
	}

	return PickPrefixMatch(idOrPrefix, prefixMatches)
}

// PickPrefixMatch returns the single ticket a prefix names. Several matches are
// an error rather than a silent pick of the first one, because the same
// resolution backs `delete` and `cancel`: a prefix that grew ambiguous as
// tickets were added must not quietly act on a different ticket than last time.
func PickPrefixMatch(idOrPrefix string, matches []string) (string, error) {
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("ticket %q not found", idOrPrefix)
	case 1:
		return matches[0], nil
	default:
		sorted := slices.Clone(matches)
		slices.Sort(sorted)
		return "", fmt.Errorf("ticket %q is ambiguous: matches %s", idOrPrefix, strings.Join(sorted, ", "))
	}
}

func (r *DiskRepo) Get(id string) (*app.StoredTicket, error) {
	if !ticket.IsSafeID(id) {
		return nil, fmt.Errorf("unsafe ticket id %q", id)
	}
	path := filepath.Join(r.dir, id+".md")
	t, err := ticket.ParseFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading ticket %s: %w", id, err)
	}
	return &app.StoredTicket{Ticket: t, FilePath: path}, nil
}

func (r *DiskRepo) List() ([]*app.StoredTicket, error) {
	paths, err := ticket.ListFiles(r.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var result []*app.StoredTicket
	for _, path := range paths {
		t, err := ticket.ParseFile(path)
		if err != nil {
			continue
		}
		if !ticket.IsCanonicalPath(path, t.ID) {
			continue
		}
		result = append(result, &app.StoredTicket{Ticket: t, FilePath: path})
	}
	return result, nil
}

func (r *DiskRepo) WithMutation(fn func(app.MutationRepository) error) error {
	return ticket.WithOfflineWrite(r.dir, func() error {
		return fn(diskMutationRepository{MutationRepository: r, dir: r.dir})
	})
}

func (r *DiskRepo) Save(st *app.StoredTicket) error {
	return r.WithMutation(func(scoped app.MutationRepository) error {
		return scoped.Save(st)
	})
}

// Reads always hit disk, including after another Save within the same scope.
// Save must not call WriteOffline, which would acquire the mutation lock again.
type diskMutationRepository struct {
	app.MutationRepository
	dir string
}

func (r diskMutationRepository) Save(st *app.StoredTicket) error {
	if st == nil || st.Ticket == nil {
		return fmt.Errorf("missing ticket")
	}
	if err := ticket.CheckBoardControl(r.dir); err != nil {
		return err
	}
	if err := ticket.RejectImportedOwnership(st.Ticket); err != nil {
		return err
	}
	if !ticket.IsSafeID(st.Ticket.ID) || filepath.Clean(filepath.Dir(st.FilePath)) != filepath.Clean(r.dir) || !ticket.IsCanonicalPath(st.FilePath, st.Ticket.ID) {
		return fmt.Errorf("ticket path outside repository")
	}
	current, err := r.Get(st.Ticket.ID)
	if err != nil {
		return err
	}
	if current.Ticket.ID != st.Ticket.ID {
		return fmt.Errorf("ticket id does not match destination")
	}
	if err := ticket.RejectImportedOwnership(current.Ticket); err != nil {
		return err
	}
	out, err := st.Ticket.Marshal()
	if err != nil {
		return fmt.Errorf("marshalling ticket: %w", err)
	}
	// Preserve WriteOffline's validation of the actual serialized document.
	parsed, err := ticket.ParseBytes(out)
	if err != nil {
		return err
	}
	if err := ticket.RejectImportedOwnership(parsed); err != nil {
		return err
	}
	if parsed.ID != st.Ticket.ID {
		return fmt.Errorf("ticket id does not match destination")
	}
	return os.WriteFile(st.FilePath, out, 0o644)
}
