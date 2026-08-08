package web

import (
	"bufio"
	"compress/gzip"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
)

// gzipMinSize is the response size below which compressing is a loss: the
// gzip header and trailer cost about 20 bytes, and a packet is going out
// either way.
const gzipMinSize = 1024

// gzipWriterPool reuses the compressor state across requests. A gzip.Writer
// holds a 32KB+ window, so allocating one per response would make the ticket
// list the daemon's largest source of garbage.
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// gzipMiddleware compresses JSON API responses. It covers /api/ only: the
// static UI answers from its own table of pre-compressed bytes, and buffering
// those a second time here would cost a copy of the whole file per request.
//
// The two streaming endpoints under /api/ are left alone. The event stream
// sends a few hundred bytes at a time and needs each one to reach the browser
// immediately, and the terminal WebSocket hijacks the connection, which a
// wrapper cannot survive.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !compressiblePath(r.URL.Path) || !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

func compressiblePath(p string) bool {
	return strings.HasPrefix(p, "/api/") && p != "/api/events"
}

// gzipResponseWriter defers the compress-or-not decision until it has seen the
// status, the content type, and enough of the body to know its size. Until
// then writes accumulate in buf, so a small response passes through untouched.
type gzipResponseWriter struct {
	http.ResponseWriter

	status  int
	buf     []byte
	decided bool
	zw      *gzip.Writer
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.status != 0 {
		return
	}
	g.status = status
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if g.status == 0 {
		g.status = http.StatusOK
	}
	if g.decided {
		if g.zw != nil {
			return g.zw.Write(p)
		}
		return g.ResponseWriter.Write(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) >= gzipMinSize {
		g.decide()
	}
	return len(p), nil
}

// decide picks the encoding, writes the header, and flushes whatever the
// buffer holds. Everything after this goes straight through.
func (g *gzipResponseWriter) decide() {
	g.decided = true
	h := g.Header()
	if len(g.buf) >= gzipMinSize && h.Get("Content-Encoding") == "" && compressibleType(h.Get("Content-Type")) {
		h.Set("Content-Encoding", "gzip")
		h.Set("Vary", "Accept-Encoding")
		// The length of the compressed body is not known yet, and the value
		// the handler set describes the uncompressed one.
		h.Del("Content-Length")
		g.zw, _ = gzipWriterPool.Get().(*gzip.Writer)
		g.zw.Reset(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(g.status)
	if len(g.buf) == 0 {
		return
	}
	if g.zw != nil {
		_, _ = g.zw.Write(g.buf)
	} else {
		_, _ = g.ResponseWriter.Write(g.buf)
	}
	g.buf = nil
}

func (g *gzipResponseWriter) close() {
	if !g.decided {
		if g.status == 0 {
			g.status = http.StatusOK
		}
		g.decide()
	}
	if g.zw != nil {
		_ = g.zw.Close()
		gzipWriterPool.Put(g.zw)
		g.zw = nil
	}
}

// Flush settles the encoding first: a handler that flushes before the buffer
// fills still expects its bytes to reach the client.
func (g *gzipResponseWriter) Flush() {
	if !g.decided {
		g.decide()
	}
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands back the raw connection untouched. Nothing under this
// middleware hijacks today, since the WebSocket path is excluded, but dropping
// the interface silently would break it if that ever changed.
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := g.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	return hj.Hijack()
}

// compressibleType reports whether a media type is worth gzipping. Everything
// the API returns is JSON; the rest of the list keeps the check honest if a
// handler starts serving text.
func compressibleType(ct string) bool {
	base, _, _ := strings.Cut(ct, ";")
	base = strings.TrimSpace(strings.ToLower(base))
	switch base {
	case "application/json", "text/plain", "text/html", "text/css", "text/javascript", "image/svg+xml":
		return true
	}
	return false
}
