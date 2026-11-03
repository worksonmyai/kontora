package remote

import (
	"net/http"
	"strings"
)

type mutationTransport struct {
	base  http.RoundTripper
	guard func(func() error) error
}

// GuardMutations applies a local board guard without changing remote-only clients.
func (c *Client) GuardMutations(guard func(func() error) error) *Client {
	base := c.hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.hc.Transport = mutationTransport{base: base, guard: guard}
	return c
}

func (t mutationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet || !strings.HasPrefix(req.URL.Path, "/api/tickets") {
		return t.base.RoundTrip(req)
	}
	var response *http.Response
	err := t.guard(func() error {
		var err error
		response, err = t.base.RoundTrip(req) //nolint:bodyclose // The caller owns the returned response body.
		return err
	})
	return response, err
}
