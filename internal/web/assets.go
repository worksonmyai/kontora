package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
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
	gzipBody     []byte // nil when the file is already compressed or gzip grew it
	gzipETag     string
}

// loadAssets builds the table on first use. The daemon calls it at startup, so
// no request pays for it; keeping it out of package init matters because the
// CLI commands link this package too and would otherwise gzip the whole UI on
// every invocation.
var loadAssets = sync.OnceValue(buildAssets)

func buildAssets() map[string]*staticAsset {
	assets := make(map[string]*staticAsset)
	_ = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, "static/")
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
		if gz := gzipAsset(name, body); gz != nil {
			a.gzipBody = gz
			a.gzipETag = ContentETag(body, "-gz")
		}
		assets[name] = a
		return nil
	})
	return assets
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

// gzipAsset returns the compressed form of an asset, or nil when it should be
// served as-is. woff2 carries its own compression, and running it through gzip
// buys under a percent while making the browser undo a second layer.
func gzipAsset(name string, body []byte) []byte {
	switch path.Ext(name) {
	case ".html", ".js", ".mjs", ".css":
	default:
		return nil
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(body); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	if buf.Len() >= len(body) {
		return nil
	}
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
	if a == nil {
		http.NotFound(w, r)
		return
	}

	h := w.Header()
	h.Set("Cache-Control", a.cacheControl)
	body, etag := a.body, a.etag
	sendGzip := false
	if a.gzipBody != nil {
		// Which of the two representations we send depends on the request
		// header, so any cache in between has to key on it as well.
		h.Set("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			body, etag, sendGzip = a.gzipBody, a.gzipETag, true
		}
	}
	h.Set("Etag", etag)

	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	h.Set("Content-Type", a.contentType)
	if sendGzip {
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
