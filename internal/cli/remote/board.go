package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/web"
)

type BoardProbeError struct{ err error }

func (e *BoardProbeError) Error() string { return "cannot verify daemon: " + e.err.Error() }
func (e *BoardProbeError) Unwrap() error { return e.err }

// Cause omits the request address for callers that print it separately.
func (e *BoardProbeError) Cause() error {
	if request, ok := errors.AsType[*url.Error](e.err); ok {
		if op, ok := request.Err.(*net.OpError); ok {
			return fmt.Errorf("%s %s: %w", op.Op, op.Net, op.Err)
		}
		return request.Err
	}
	return e.err
}

// VerifyBoard is for local daemons: directory aliases match by filesystem identity.
func VerifyBoard(ctx context.Context, c *Client, ticketsDir string) (web.ConfigInfo, error) {
	identity, err := c.ConfigContext(ctx)
	if err != nil {
		return web.ConfigInfo{}, &BoardProbeError{err: err}
	}
	if !filepath.IsAbs(identity.TicketsDir) {
		return web.ConfigInfo{}, fmt.Errorf("daemon directory identity missing")
	}
	matches, err := config.SameDirectory(ticketsDir, identity.TicketsDir)
	if err != nil {
		return web.ConfigInfo{}, fmt.Errorf("cannot compare daemon board %q with requested board %q: %w", identity.TicketsDir, ticketsDir, err)
	}
	if !matches {
		return web.ConfigInfo{}, fmt.Errorf("daemon board %q does not match requested board %q", identity.TicketsDir, ticketsDir)
	}
	return identity, nil
}
