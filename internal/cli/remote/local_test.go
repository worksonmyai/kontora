package remote

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/web"
)

func TestNewLocal(t *testing.T) {
	for _, tc := range []struct {
		host string
		want string
	}{
		{"", "127.0.0.1"}, {"localhost", "127.0.0.1"}, {"LOCALHOST", "127.0.0.1"},
		{"0.0.0.0", "127.0.0.1"}, {"::", "::1"},
		{"127.0.0.1", "127.0.0.1"}, {"127.0.0.2", "127.0.0.2"}, {"::1", "::1"},
		{"example.com", ""}, {"192.168.1.2", ""}, {"8.8.8.8", ""},
		{"localhost.example.com", ""}, {"[::1]", ""},
	} {
		t.Run(tc.host, func(t *testing.T) {
			client, err := NewLocal(tc.host, 8080, "secret")
			if tc.want == "" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "http://"+net.JoinHostPort(tc.want, "8080"), client.Base())
			transport := client.hc.Transport.(*http.Transport)
			assert.Nil(t, transport.Proxy)
			_, err = transport.DialContext(t.Context(), "tcp", "example.com:80")
			require.ErrorContains(t, err, "refusing non-loopback")
		})
	}
}

func TestClientConfigRedirectCompatibility(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_ = json.NewEncoder(w).Encode(web.ConfigInfo{TicketsDir: "/board"})
	}))
	defer target.Close()

	for _, status := range []int{300, 301, 302, 303, 304, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", target.URL+"/api/config")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(web.ConfigInfo{TicketsDir: "/board"})
			}))
			defer srv.Close()
			port := srv.Listener.Addr().(*net.TCPAddr).Port
			local, err := NewLocal("localhost", port, "")
			require.NoError(t, err)
			_, err = local.ConfigContext(t.Context())
			require.Error(t, err)
			assert.Zero(t, reached.Load())
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/config", http.StatusFound)
	}))
	defer srv.Close()
	cfg, err := New(srv.URL, "").Config()
	require.NoError(t, err)
	assert.Equal(t, "/board", cfg.TicketsDir)
	assert.EqualValues(t, 1, reached.Load())
}

func TestClientConfigContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		body bool
	}{
		{"waiting for headers", false},
		{"waiting for body", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.body {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := New(srv.URL, "").ConfigContext(ctx)
				result <- err
			}()
			<-started
			cancel()
			require.ErrorIs(t, <-result, context.Canceled)
		})
	}
}
