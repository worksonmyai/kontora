package web

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawGet fetches a path with the request's own Accept-Encoding, so the test
// sees the encoding and the bytes actually sent. Transport.RoundTrip is used
// directly, and Accept-Encoding is always set, because http.Client asks for
// gzip on its own whenever the caller left the header off and then unwraps the
// reply, hiding the very thing under test. An identityHeader is therefore a
// real identity request, not an absent one.
func rawGet(t *testing.T, srv *Server, path string, header http.Header) rawResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+path, nil)
	require.NoError(t, err)
	if header == nil {
		header = identityHeader()
	}
	maps.Copy(req.Header, header)
	resp, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return rawResult{status: resp.StatusCode, header: resp.Header, body: body}
}

// rawResult is what the caller needs after rawGet has drained and closed the
// response: the status, the headers, and the bytes as they came off the wire.
type rawResult struct {
	status int
	header http.Header
	body   []byte
}

func (r rawResult) get(name string) string { return r.header.Get(name) }

// gunzip fails the test rather than returning an error, so callers stay flat.
func gunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	require.NoError(t, err)
	plain, err := io.ReadAll(zr)
	require.NoError(t, err)
	return plain
}

func gzipHeader() http.Header {
	return http.Header{"Accept-Encoding": {"gzip"}}
}

func identityHeader() http.Header {
	return http.Header{"Accept-Encoding": {"identity"}}
}

// TestStaticAssets_Compression checks that the text of the UI goes out gzipped
// and the already-compressed fonts do not, and that both carry a length the
// browser can use.
func TestStaticAssets_Compression(t *testing.T) {
	srv := newTestServer(t)

	cases := []struct {
		name     string
		path     string
		wantGzip bool
	}{
		{"html", "/", true},
		{"app script", "/app.js", true},
		{"stylesheet", "/app.css", true},
		{"es module", "/vendor/yaml@2.8.1/yaml.mjs", true},
		{"font", "/vendor/fonts/dm-sans-latin.woff2", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := rawGet(t, srv, tc.path, gzipHeader())
			require.Equal(t, http.StatusOK, resp.status)
			assert.Equal(t, strconv.Itoa(len(resp.body)), resp.get("Content-Length"),
				"Content-Length must describe the bytes actually sent")

			if !tc.wantGzip {
				assert.Empty(t, resp.get("Content-Encoding"))
				return
			}
			require.Equal(t, "gzip", resp.get("Content-Encoding"))
			assert.Equal(t, "Accept-Encoding", resp.get("Vary"))

			plain := gunzip(t, resp.body)
			assert.Less(t, len(resp.body), len(plain), "gzip must be smaller than the source")

			// A client that takes no encoding gets the same document.
			identity := rawGet(t, srv, tc.path, nil)
			require.Equal(t, http.StatusOK, identity.status)
			assert.Empty(t, identity.get("Content-Encoding"))
			assert.Equal(t, string(plain), string(identity.body))
		})
	}
}

// TestStaticAssets_Revalidation covers the repeat visit: the browser sends back
// the ETag it stored and gets a 304 with no body.
func TestStaticAssets_Revalidation(t *testing.T) {
	srv := newTestServer(t)

	for _, path := range []string{"/", "/app.js", "/vendor/fonts/dm-sans-latin.woff2"} {
		t.Run(path, func(t *testing.T) {
			first := rawGet(t, srv, path, gzipHeader())
			require.Equal(t, http.StatusOK, first.status)
			etag := first.get("Etag")
			require.NotEmpty(t, etag)

			h := gzipHeader()
			h.Set("If-None-Match", etag)
			second := rawGet(t, srv, path, h)
			assert.Equal(t, http.StatusNotModified, second.status)
			assert.Empty(t, second.body)

			// A stale ETag has to return the document again.
			h.Set("If-None-Match", `"stale"`)
			assert.Equal(t, http.StatusOK, rawGet(t, srv, path, h).status)
		})
	}
}

// TestStaticAssets_GzipETagDiffers guards the two representations against being
// confused for each other: a cache holding the gzip copy must not satisfy a
// client that cannot decode it.
func TestStaticAssets_GzipETagDiffers(t *testing.T) {
	srv := newTestServer(t)

	gz := rawGet(t, srv, "/app.js", gzipHeader()).get("Etag")
	plain := rawGet(t, srv, "/app.js", nil).get("Etag")
	require.NotEmpty(t, gz)
	require.NotEmpty(t, plain)
	assert.NotEqual(t, gz, plain)

	h := identityHeader()
	h.Set("If-None-Match", gz)
	assert.Equal(t, http.StatusOK, rawGet(t, srv, "/app.js", h).status,
		"the gzip ETag must not match an identity request")
}

// TestStaticAssets_CacheControl checks the split between the vendored
// libraries, which pin a version in their path and so never change under that
// URL, and everything the build rewrites in place.
func TestStaticAssets_CacheControl(t *testing.T) {
	srv := newTestServer(t)

	cases := []struct {
		path string
		want string
	}{
		{"/vendor/alpinejs@3.14.8/cdn.min.js", cacheImmutable},
		{"/vendor/xterm@5.5.0/xterm.css", cacheImmutable},
		{"/", cacheRevalidate},
		{"/app.js", cacheRevalidate},
		{"/settings.js", cacheRevalidate},
		{"/stats.js", cacheRevalidate},
		{"/vendor/fonts/fonts.css", cacheRevalidate},
		{"/vendor/fonts/dm-sans-latin.woff2", cacheRevalidate},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp := rawGet(t, srv, tc.path, gzipHeader())
			require.Equal(t, http.StatusOK, resp.status)
			assert.Equal(t, tc.want, resp.get("Cache-Control"))
		})
	}
}

// TestAPI_Compression covers the ticket list, the largest thing the daemon
// sends and the one a browser re-reads on every load.
func TestAPI_Compression(t *testing.T) {
	tickets := make([]TicketInfo, 200)
	for i := range tickets {
		tickets[i] = TicketInfo{
			ID:     fmt.Sprintf("tkt-%04d", i),
			Title:  fmt.Sprintf("Ticket number %d that needs some work doing to it", i),
			Status: "todo",
			Path:   "~/projects/kontora",
			Agent:  "claude",
		}
	}
	srv := startHandlerTestServer(t, &mockService{tickets: tickets})

	resp := rawGet(t, srv, "/api/tickets", gzipHeader())
	require.Equal(t, http.StatusOK, resp.status)
	require.Equal(t, "gzip", resp.get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", resp.get("Vary"))

	plain := gunzip(t, resp.body)
	assert.Contains(t, string(plain), "tkt-0199")
	assert.Less(t, len(resp.body), len(plain)/2, "the ticket list should compress well past half")

	// A client that cannot decode gzip still gets readable JSON.
	identity := rawGet(t, srv, "/api/tickets", nil)
	assert.Empty(t, identity.get("Content-Encoding"))
	assert.Equal(t, string(plain), string(identity.body))
}

// TestAPI_SmallResponsesStayUncompressed keeps gzip off the replies that are
// shorter than its own framing.
func TestAPI_SmallResponsesStayUncompressed(t *testing.T) {
	srv := startHandlerTestServer(t, &mockService{})

	resp := rawGet(t, srv, "/api/tickets", gzipHeader())
	require.Equal(t, http.StatusOK, resp.status)
	assert.Empty(t, resp.get("Content-Encoding"))
	assert.Contains(t, string(resp.body), "tickets")
}

// TestSSE_NotCompressed is the reason the middleware skips /api/events:
// buffering the stream to compress it would hold each event back.
func TestSSE_NotCompressed(t *testing.T) {
	broker := NewSSEBroker()
	srv := startHandlerTestServerWithBroker(t, &mockService{}, broker)

	req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/api/events", nil)
	require.NoError(t, err)
	// Set by hand so http.Client leaves the reply encoded instead of asking for
	// gzip itself and unwrapping the answer before the test can look at it.
	req.Header.Set("Accept-Encoding", "gzip")
	// The timeout covers reading the body, so a stream that never arrives fails
	// here rather than hanging until the whole test binary times out.
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Content-Encoding"))
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	go func() {
		time.Sleep(20 * time.Millisecond)
		broker.Broadcast(TicketEvent{Type: "ticket_updated", Ticket: TicketInfo{ID: "tkt-1"}})
	}()

	// One event reaches the client on its own, without a buffer having to fill.
	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	require.NoError(t, err, "no event arrived; the stream is being buffered")
	assert.Contains(t, string(buf[:n]), "tkt-1")
}

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"gzip, deflate, br", true},
		{"deflate, gzip;q=1.0, *;q=0.5", true},
		{"br", false},
		{"gzip;q=0", false},
		{"GZIP", true},
		{" gzip ", true},
	}

	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "/", nil)
			require.NoError(t, err)
			if tc.header != "" {
				r.Header.Set("Accept-Encoding", tc.header)
			}
			assert.Equal(t, tc.want, acceptsGzip(r))
		})
	}
}

func TestETagMatches(t *testing.T) {
	cases := []struct {
		name   string
		header string
		etag   string
		want   bool
	}{
		{"empty header", "", `"abc"`, false},
		{"exact", `"abc"`, `"abc"`, true},
		{"different", `"xyz"`, `"abc"`, false},
		{"in a list", `"one", "abc", "two"`, `"abc"`, true},
		{"wildcard", "*", `"abc"`, true},
		{"weak client tag", `W/"abc"`, `"abc"`, true},
		{"weak server tag", `"abc"`, `W/"abc"`, true},
		{"gzip suffix is a different entity", `"abc"`, `"abc-gz"`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, etagMatches(tc.header, tc.etag))
		})
	}
}
