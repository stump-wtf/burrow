package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// main used to build http.Server{Addr, Handler} and nothing else, so a client
// that never finished its request headers held its connection for as long as
// it liked.
func TestNewServerBoundsSlowClients(t *testing.T) {
	srv := newServer("127.0.0.1:0", http.NotFoundHandler())

	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout must be set: without it a client that never finishes its headers is never cut off")
	}
	if srv.ReadTimeout < srv.ReadHeaderTimeout {
		t.Fatalf("ReadTimeout (%s) covers the headers and the body, so it cannot be shorter than ReadHeaderTimeout (%s)", srv.ReadTimeout, srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout must be set: without it an idle keep-alive connection is held open indefinitely")
	}
	// A WriteTimeout runs from the end of the request headers, so it bounds
	// the handler too. A Library request can wait on a git clone that has no
	// deadline, and a provider listing on one gateway call of up to 10 s per
	// row; any number here would cut one of them.
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout is %s: it would cut handlers that legitimately run long; see newServer", srv.WriteTimeout)
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// The read timeouts bound how long a request may take to arrive, not how long
// its handler may run: a handler that outlives them must not see its context
// cancelled. This is what lets ReadTimeout sit far below the slowest handler.
func TestSlowHandlerOutlivesReadTimeout(t *testing.T) {
	const arrival = 250 * time.Millisecond
	srv := newServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			http.Error(w, "cancelled", http.StatusInternalServerError)
		case <-time.After(3 * arrival):
			_, _ = io.WriteString(w, "done")
		}
	}))
	srv.ReadHeaderTimeout = arrival
	srv.ReadTimeout = arrival

	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, time.Second) }()
	t.Cleanup(func() {
		cancel()
		<-served
	})

	// With a body the deadline is lifted when the body ends; without one,
	// as soon as the headers are in.
	t.Run("group", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method, func(t *testing.T) {
				t.Parallel()
				var body io.Reader
				if method == http.MethodPost {
					body = strings.NewReader(`{"scopes":[]}`)
				}
				req, err := http.NewRequest(method, "http://"+ln.Addr().String()+"/", body)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := http.DefaultTransport.RoundTrip(req)
				if err != nil {
					t.Fatalf("a handler slower than ReadTimeout must still answer: %v", err)
				}
				answer, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || string(answer) != "done" {
					t.Fatalf("want 200 done, got %d %q", resp.StatusCode, answer)
				}
			})
		}
	})
}

// The image ships on every merge to main, and the process used to die on
// SIGTERM with whatever it was serving. serve stops accepting when its context
// ends and lets requests already in flight finish.
func TestServeDrainsInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := newServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	}))

	ln := listen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, 5*time.Second) }()

	type result struct {
		status int
		body   string
		err    error
	}
	answered := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			answered <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		answered <- result{status: resp.StatusCode, body: string(body)}
	}()
	<-started

	cancel() // the signal

	// New connections are refused once shutdown has begun…
	refused := false
	for i := 0; i < 200 && !refused; i++ {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			refused = true
			break
		}
		conn.Close()
		time.Sleep(5 * time.Millisecond)
	}
	if !refused {
		t.Fatalf("serve kept accepting connections after its context ended")
	}
	// …while the request in flight is still being waited on.
	select {
	case err := <-served:
		t.Fatalf("serve returned (%v) with a request still in flight", err)
	default:
	}

	close(release)
	got := <-answered
	if got.err != nil || got.status != http.StatusOK || got.body != "finished" {
		t.Fatalf("the in-flight request must complete: status %d body %q err %v", got.status, got.body, got.err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("a clean shutdown must return nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("serve did not return after the last request finished")
	}
}

// The wait is bounded: a request that will not finish is cut when the grace
// period runs out, so a stuck handler cannot hold a deploy.
func TestServeStopsWaitingAfterGrace(t *testing.T) {
	started := make(chan struct{})
	srv := newServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))

	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, 100*time.Millisecond) }()

	clientDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		clientDone <- err
	}()
	<-started

	begin := time.Now()
	cancel()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatalf("serve was still waiting %s after a 100ms grace period", time.Since(begin))
	}
	select {
	case <-clientDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("the stuck request's connection was not closed when the grace period ran out")
	}
}
