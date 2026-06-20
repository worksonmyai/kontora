//go:build !windows

package remote

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// watchWinch calls onResize with the current terminal size whenever the window
// changes (SIGWINCH). It returns a function that stops watching.
func watchWinch(ctx context.Context, fd int, onResize func(cols, rows int)) func() {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
				if w, h, err := term.GetSize(fd); err == nil {
					onResize(w, h)
				}
			}
		}
	}()
	return func() { signal.Stop(winch) }
}
