package watcher

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Op int

const (
	OpChanged Op = iota // create or write
	OpRemoved           // remove or rename
)

type Event struct {
	Path string
	Op   Op
}

type Watcher struct {
	fsw      *fsnotify.Watcher
	events   chan Event
	errors   chan error
	debounce time.Duration

	mu     sync.Mutex
	timers map[string]*time.Timer

	done chan struct{}
}

func New(dir string, debounce time.Duration) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := fsw.Add(dir); err != nil {
		fsw.Close()
		return nil, err
	}

	w := &Watcher{
		fsw:      fsw,
		events:   make(chan Event, 64),
		errors:   make(chan error, 8),
		debounce: debounce,
		timers:   make(map[string]*time.Timer),
		done:     make(chan struct{}),
	}
	go w.loop()
	return w, nil
}

func (w *Watcher) Events() <-chan Event { return w.events }
func (w *Watcher) Errors() <-chan error { return w.errors }

func (w *Watcher) Close() error {
	err := w.fsw.Close()
	<-w.done
	w.mu.Lock()
	for _, t := range w.timers {
		t.Stop()
	}
	w.mu.Unlock()
	return err
}

func (w *Watcher) loop() {
	defer close(w.done)
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.errors <- err
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	if !isMD(ev.Name) {
		return
	}

	path, err := filepath.Abs(ev.Name)
	if err != nil {
		path = ev.Name
	}

	var op Op
	switch {
	case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
		op = OpRemoved
	case ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write):
		op = OpChanged
	default:
		return
	}

	w.debounceEmit(path, op)
}

func (w *Watcher) debounceEmit(path string, op Op) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.timers[path]; ok {
		t.Stop()
	}

	w.timers[path] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()

		w.events <- Event{Path: path, Op: op}
	})
}

func isMD(path string) bool {
	return strings.HasSuffix(path, ".md")
}
