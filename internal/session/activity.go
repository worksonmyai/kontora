package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/worksonmyai/kontora/internal/logfmt"
)

// Artifact owns an open descriptor. The caller must close File.
type Artifact struct {
	File   *os.File
	Info   os.FileInfo
	Path   string
	Source string
}

// OpenActivity opens the requested run's sidecar or the stage's shared log.
// The shared log may belong to a newer run. A malformed sidecar is an error
// when read; callers must not fall back to plaintext or another run's sidecar.
func OpenActivity(logsDir, id, stage string, run int) (*Artifact, error) {
	return OpenActivityLog(logsDir, id, stage, run, LogPath(logsDir, id, stage))
}

// OpenActivityLog uses logPath as the fallback, including graph visit logs.
// Both artifacts must remain within the ticket's log directory.
func OpenActivityLog(logsDir, id, stage string, run int, logPath string) (*Artifact, error) {
	if stage == "" {
		return nil, fmt.Errorf("activity stage is empty: %w", os.ErrNotExist)
	}
	dir := TicketDir(logsDir, id)
	a, err := openArtifact(dir, EventsPath(logsDir, id, stage, run), "events")
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return a, err
	}
	return openArtifact(dir, logPath, "log")
}

// OpenNewestLog requires an already resolved ticket ID.
func OpenNewestLog(logsDir, id string) (*Artifact, error) {
	dir := filepath.Join(logsDir, id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var newest string
	var newestTime int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.ModTime().UnixNano() > newestTime {
			newest = entry.Name()
			newestTime = info.ModTime().UnixNano()
		}
	}
	if newest == "" {
		return nil, os.ErrNotExist
	}
	return openArtifact(dir, filepath.Join(dir, newest), "log")
}

func openArtifact(dir, path, source string) (*Artifact, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	name, err := filepath.Rel(dir, path)
	if err != nil {
		return nil, err
	}
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("log path %q is outside ticket directory", path)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Artifact{File: f, Info: st, Path: path, Source: source}, nil
}

func FileVersion(path string, st os.FileInfo) string {
	var identity string
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		identity = fmt.Sprintf("%d|%d", s.Dev, s.Ino)
	}
	return fmt.Sprintf("%s|%s|%d|%d", filepath.Clean(path), identity, st.Size(), st.ModTime().UnixNano())
}

func (a *Artifact) Tape() (*logfmt.Tape, error) {
	var tape logfmt.Tape
	dec := json.NewDecoder(a.File)
	if err := dec.Decode(&tape); err != nil {
		return nil, fmt.Errorf("parsing activity sidecar %s: %w", a.Path, err)
	}
	// Reject trailing data just as json.Unmarshal does.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("parsing activity sidecar %s: trailing data", a.Path)
	}
	return &tape, nil
}

// Content reads at most tail bytes when tail is positive. Start and size are
// byte offsets in the full file, before UTF-8 decoding by the HTTP client.
func (a *Artifact) Content(tail int) (content string, start, size int64, err error) {
	size = a.Info.Size()
	var data []byte
	for {
		start = 0
		if tail > 0 && size > int64(tail) {
			start = size - int64(tail)
		}
		data = make([]byte, size-start)
		n, readErr := a.File.ReadAt(data, start)
		if readErr == nil {
			break
		}
		if !errors.Is(readErr, io.EOF) {
			return "", 0, 0, readErr
		}
		st, statErr := a.File.Stat()
		if statErr != nil {
			return "", 0, 0, statErr
		}
		// Materialization truncates the shared log in place. Recompute the suffix
		// after shrinkage, without changing the snapshot used to validate it.
		if st.Size() < size {
			size = st.Size()
			continue
		}
		data = data[:n]
		size = start + int64(n)
		break
	}
	if start > 0 {
		for len(data) > 0 && !utf8.RuneStart(data[0]) {
			data = data[1:]
			start++
		}
	}
	return string(data), start, size, nil
}
