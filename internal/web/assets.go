package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// assetContentTypes maps the file extensions the embedded UI ships to their
// MIME types. They are spelled out rather than resolved through
// mime.TypeByExtension because only a handful of types come from Go's builtin
// table; the rest are read from the host's mime database, which does not carry
// woff2 everywhere. A missing font type makes the browser refuse the font.
var assetContentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".woff2": "font/woff2",
}

const (
	// cacheImmutable is for URLs whose bytes can never change: the vendored
	// libraries, which pin their version in the path.
	cacheImmutable = "public, max-age=31536000, immutable"
	// cacheRevalidate lets the browser keep the bytes but makes it ask first.
	// A rebuilt binary changes the ETag, so the answer is a 304 until the UI
	// actually changes, and the reply carries no body either way.
	cacheRevalidate = "no-cache"

	// uiBundleAsset is the name the compiled UI modules are served under. It
	// is the one asset with no file behind it.
	uiBundleAsset = "app.js"
)

// staticAsset is one embedded UI file, read into memory once with everything a
// response needs: the bytes, their ETag, and a gzip copy for clients that take
// one. Compressing at startup instead of per request makes serving the UI a
// header write plus one memory copy.
type staticAsset struct {
	contentType  string
	cacheControl string
	body         []byte
	etag         string
	// gzipETag is empty when the asset goes out as-is: already-compressed
	// bytes, or a file gzip does not shrink. gzipBody is then nil too.
	gzipETag string
	// gzipBody yields the compressed copy. Table assets hand back bytes
	// compressed at startup; the dev bundle defers the work, so a 304 or an
	// identity request does not pay for it.
	gzipBody func() []byte
}

// loadAssets builds the table on first use. The daemon calls it at startup, so
// no request pays for it; keeping it out of package init matters because the
// CLI commands link this package too and would otherwise gzip the whole UI and
// compile the bundle on every invocation.
var loadAssets = sync.OnceValues(buildAssets)

func buildAssets() (map[string]*staticAsset, error) {
	assets := make(map[string]*staticAsset)
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, "static/")
		assets[name] = newAsset(name, body)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// app.js is not a file on disk: it is the UI modules compiled to one
	// script. Compiling it here is what makes a binary whose embedded JS does
	// not build fail at startup instead of at the first request. serveAsset
	// resolves the asset itself, so under KONTORA_WEB_DIR the table entry is
	// left out rather than freezing one reading of a working copy.
	if devWebDir() == "" {
		a, err := embeddedUIAsset()
		if err != nil {
			return nil, err
		}
		assets[uiBundleAsset] = a
	}

	return assets, nil
}

// embeddedUIAsset is the compiled-in bundle, packed for serving once.
var embeddedUIAsset = sync.OnceValues(func() (*staticAsset, error) {
	bundle, err := embeddedUIBundle()
	if err != nil {
		return nil, err
	}
	return newAsset(uiBundleAsset, bundle), nil
})

// uiAsset is the /app.js response: the working copy compiled fresh when
// KONTORA_WEB_DIR is set, and the bundle packed at startup otherwise.
func uiAsset() (*staticAsset, error) {
	if devWebDir() == "" {
		return embeddedUIAsset()
	}
	bundle, err := uiBundle()
	if err != nil {
		return nil, err
	}
	return newLazyAsset(uiBundleAsset, bundle), nil
}

// devAsset reads one file of the working copy under KONTORA_WEB_DIR, so an
// edit to index.html or a `make css` run reaches a reload the same way a JS
// edit does. It returns nil for anything it cannot read, which keeps the
// embedded copy serving.
//
// Only a name the embedded table already carries gets here, so a request path
// cannot pick the file: the caller looked it up first.
func devAsset(name string) *staticAsset {
	body, err := os.ReadFile(filepath.Join(devWebDir(), "internal", "web", "static", filepath.FromSlash(name)))
	if err != nil {
		return nil
	}
	return newLazyAsset(name, body)
}

// newAsset packs one file into everything a response needs, compressing now.
func newAsset(name string, body []byte) *staticAsset {
	a := baseAsset(name, body)
	if gz := gzipAsset(name, body); gz != nil {
		a.gzipETag = ContentETag(body, "-gz")
		a.gzipBody = func() []byte { return gz }
	}
	return a
}

// newLazyAsset packs a body built inside the request path, which is how the
// KONTORA_WEB_DIR assets are served. gzip costs more than everything else in
// that path put together, so it waits until the response is known to need it:
// a 304 or an identity request never pays. The size check newAsset makes goes
// with it, so a file gzip would grow is still sent compressed here.
func newLazyAsset(name string, body []byte) *staticAsset {
	a := baseAsset(name, body)
	if compressibleAsset(name) {
		a.gzipETag = ContentETag(body, "-gz")
		a.gzipBody = func() []byte { return gzipBytes(body) }
	}
	return a
}

func baseAsset(name string, body []byte) *staticAsset {
	a := &staticAsset{
		contentType:  assetContentTypes[path.Ext(name)],
		cacheControl: cacheRevalidate,
		body:         body,
		etag:         ContentETag(body, ""),
	}
	if a.contentType == "" {
		a.contentType = "application/octet-stream"
	}
	if versionedVendorPath(name) {
		a.cacheControl = cacheImmutable
	}
	return a
}

// versionedVendorPath reports whether a path pins the version of what it
// serves, as vendor/<lib>@<version>/... does. Bumping such a library moves it
// to a new URL, so the old one can be cached forever. vendor/fonts/ carries no
// version and keeps revalidating.
func versionedVendorPath(name string) bool {
	rest, ok := strings.CutPrefix(name, "vendor/")
	if !ok {
		return false
	}
	dir, _, ok := strings.Cut(rest, "/")
	return ok && strings.Contains(dir, "@")
}

// ContentETag is a strong validator over the given bytes. The suffix
// distinguishes the gzip representation of an asset from the identity one,
// which HTTP treats as two different entities.
func ContentETag(body []byte, suffix string) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + suffix + `"`
}

// compressibleAsset reports whether gzip is worth trying. woff2 carries its own
// compression, and running it through gzip buys under a percent while making
// the browser undo a second layer.
func compressibleAsset(name string) bool {
	switch path.Ext(name) {
	case ".html", ".js", ".mjs", ".css":
		return true
	}
	return false
}

// gzipAsset returns the compressed form of an asset, or nil when it should be
// served as-is.
func gzipAsset(name string, body []byte) []byte {
	if !compressibleAsset(name) {
		return nil
	}
	gz := gzipBytes(body)
	if len(gz) >= len(body) {
		return nil
	}
	return gz
}

// gzipBytes compresses body. Neither a bytes.Buffer nor a valid level fails, so
// there is no error to return.
func gzipBytes(body []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(body)
	_ = zw.Close()
	return buf.Bytes()
}

// serveAsset answers from the in-memory table: pick the representation, and
// either return a bodiless 304 or write the bytes.
//
// It writes the response itself rather than going through http.ServeContent,
// which omits Content-Length whenever Content-Encoding is set. That rule fits
// a handler holding identity bytes that something downstream will encode; here
// the gzip bytes are already in hand and their length is known, and sending it
// saves the browser a chunked body. The trade is Range support, which nothing
// asks for on a script or a stylesheet.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	a := s.assets[name]
	switch {
	case name == uiBundleAsset:
		// Under KONTORA_WEB_DIR this compiles the working copy, which is what
		// makes a browser reload pick up an edit. A build that fails must not
		// fall back to the last good bundle; the message goes to the log
		// rather than the body, because this response needs no auth.
		var err error
		if a, err = uiAsset(); err != nil {
			s.log.Error("building the web UI", "err", err)
			http.Error(w, "the web UI failed to build; see the daemon log", http.StatusInternalServerError)
			return
		}
	case a != nil && devWebDir() != "":
		if dev := devAsset(name); dev != nil {
			a = dev
		}
	}
	if a == nil {
		http.NotFound(w, r)
		return
	}

	h := w.Header()
	if name == "index.html" {
		// The one response that is a document, so the only one that needs to
		// load anything. Everything else keeps the baseline policy.
		h.Set("Content-Security-Policy", documentCSP)
	}
	h.Set("Cache-Control", a.cacheControl)
	body, etag := a.body, a.etag
	sendGzip := false
	if a.gzipETag != "" {
		// Which of the two representations we send depends on the request
		// header, so any cache in between has to key on it as well.
		h.Set("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			etag, sendGzip = a.gzipETag, true
		}
	}
	h.Set("Etag", etag)

	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	h.Set("Content-Type", a.contentType)
	if sendGzip {
		// After the 304 check: an asset that compresses in the request path
		// would otherwise do the work to send an empty body.
		body = a.gzipBody()
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	// The net/http server drops the body for a HEAD request on its own.
	_, _ = w.Write(body)
}

// etagMatches reports whether an If-None-Match header covers etag. RFC 9110
// asks for the weak comparison here, so a W/ prefix on either side makes no
// difference to the outcome.
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for candidate := range strings.SplitSeq(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == want {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether the client asked for gzip. An explicit q=0 is a
// refusal, which is how a client opts out of a coding it would otherwise get.
func acceptsGzip(r *http.Request) bool {
	for enc := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(enc, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		return strings.ReplaceAll(params, " ", "") != "q=0"
	}
	return false
}
