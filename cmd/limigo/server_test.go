package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hweyth/limigo/internal/api"
)

// TestNewServerClosesSlowHeaderConnection is the slowloris shape: a client that
// opens a connection, sends part of a request, and then goes quiet. With every
// timeout at zero the server would hold the goroutine and file descriptor
// forever; with ReadHeaderTimeout set it must drop the connection on its own.
func TestNewServerClosesSlowHeaderConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// readHeader is short so the test is fast. Every other budget is longer
	// than the client-side read deadline below, so the only thing that can
	// make the server let go in time is the header timeout itself.
	timeouts := serverTimeouts{
		readHeader: 100 * time.Millisecond,
		read:       5 * time.Second,
		write:      5 * time.Second,
		idle:       5 * time.Second,
	}
	srv := newServer(ln.Addr().String(), http.NotFoundHandler(), timeouts)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// A request line and one complete header, then silence: the headers never
	// terminate, so the server can only get past this by giving up.
	if _, err := conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: limigo\r\nX-Slow: ")); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	// Well past readHeader. If the server is still holding the connection
	// open when this expires, that is the bug.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 512)
	_, err = conn.Read(buf)
	// net/http does not reply on a header timeout; it just closes the socket,
	// which the client sees as io.EOF (or a reset on Windows). Either means
	// the server let go. Only a deadline on our side means it did not.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("server kept a slow-header connection open past %s: %v", timeouts.readHeader, err)
	}
}

// TestDefaultServerTimeouts pins the production values so a zero can never
// silently come back (net/http reads zero as "no timeout").
func TestDefaultServerTimeouts(t *testing.T) {
	srv := newServer(":0", http.NotFoundHandler(), defaultServerTimeouts)

	tests := []struct {
		name string
		got  time.Duration
	}{
		{name: "ReadHeaderTimeout", got: srv.ReadHeaderTimeout},
		{name: "ReadTimeout", got: srv.ReadTimeout},
		{name: "WriteTimeout", got: srv.WriteTimeout},
		{name: "IdleTimeout", got: srv.IdleTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got <= 0 {
				t.Fatalf("%s = %s, want > 0", tt.name, tt.got)
			}
		})
	}

	if srv.ReadHeaderTimeout > srv.ReadTimeout {
		t.Fatalf("ReadHeaderTimeout %s exceeds ReadTimeout %s; the header budget must fit inside the request budget", srv.ReadHeaderTimeout, srv.ReadTimeout)
	}
	// See defaultServerTimeouts: idle must outlast Traefik's default 90s
	// idleConnTimeout towards backends (a default, not something the compose
	// stack configures) so the proxy always hangs up first.
	if srv.IdleTimeout <= 90*time.Second {
		t.Fatalf("IdleTimeout = %s, want > 90s so limigo never closes an idle keep-alive before Traefik does", srv.IdleTimeout)
	}
}

// TestDefaultCheckTimeoutInsideWriteTimeout pins the ordering the check
// deadline exists for: the store call must give up before the server stops
// being able to write the 503, with room left for the write itself.
func TestDefaultCheckTimeoutInsideWriteTimeout(t *testing.T) {
	if defaultCheckTimeout <= 0 {
		t.Fatalf("defaultCheckTimeout = %s, want > 0", defaultCheckTimeout)
	}
	if defaultCheckTimeout >= defaultServerTimeouts.write {
		t.Fatalf("defaultCheckTimeout %s is not inside WriteTimeout %s; a hung store would reset the connection instead of returning 503", defaultCheckTimeout, defaultServerTimeouts.write)
	}
}

// TestDrainThenShutdownOrdering pins the sequence that makes /readyz useful
// to a proxy: readiness is failed first, the node keeps serving for the drain
// delay, and only then is the listener shut down. A shutdown that closed the
// listener first would leave the proxy routing to a dead replica until its
// next health check.
func TestDrainThenShutdownOrdering(t *testing.T) {
	readiness := api.NewReadiness()
	readiness.Ready()
	const delay = 50 * time.Millisecond

	var readyAtShutdown bool
	var shutdownAt time.Time
	start := time.Now()
	err := drainThenShutdown(readiness, delay, func() error {
		shutdownAt = time.Now()
		recorder := httptest.NewRecorder()
		readiness.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		readyAtShutdown = recorder.Code == http.StatusOK
		return nil
	})
	if err != nil {
		t.Fatalf("drainThenShutdown: %v", err)
	}
	if shutdownAt.IsZero() {
		t.Fatal("shutdown was never called")
	}
	if readyAtShutdown {
		t.Fatal("/readyz still returned 200 when shutdown began; readiness must fail before the listener closes")
	}
	if waited := shutdownAt.Sub(start); waited < delay {
		t.Fatalf("shutdown began %s after Draining, want at least the %s drain delay", waited, delay)
	}
}
