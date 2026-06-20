//go:build windows

package remote

import "context"

// watchWinch is a no-op on Windows, which has no SIGWINCH. The terminal size is
// sent once on connect; live resize is not supported.
func watchWinch(_ context.Context, _ int, _ func(cols, rows int)) func() {
	return func() {}
}
