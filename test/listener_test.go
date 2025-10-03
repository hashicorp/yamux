package yamux_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	yamux "github.com/hashicorp/yamux"
)

// TestYamuxListener_AcceptUnblocksOnClose ensures Accept returns when the
// session is closed, which matches net.Listener semantics and is relied upon
// by reverse SOCKS5 servers using yamux as a listener.
func TestYamuxListener_AcceptUnblocksOnClose(t *testing.T) {
	c, s := testConnPipe(t)
	client, server := testClientServerConfig(t, c, s, testConfNoKeepAlive(), testConfNoKeepAlive())
	_ = client

	errCh := make(chan error, 1)
	go func() {
		_, err := server.Accept()
		errCh <- err
	}()

	// Give Accept a moment to block.
	time.Sleep(20 * time.Millisecond)
	_ = server.Close()

	select {
	case err := <-errCh:
		if !errors.Is(err, yamux.ErrSessionShutdown) {
			t.Fatalf("expected ErrSessionShutdown from Accept after Close, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Accept to unblock after Close")
	}
}

// TestAcceptWithContextTimeout verifies AcceptStreamWithContext respects the
// provided context and times out when no streams arrive.
func TestAcceptWithContextTimeout(t *testing.T) {
	c, s := testConnPipe(t)
	_, server := testClientServerConfig(t, c, s, testConfNoKeepAlive(), testConfNoKeepAlive())

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := server.AcceptStreamWithContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got: %v", err)
	}
}

// TestStreamAddrFallback verifies that LocalAddr/RemoteAddr on sessions and
// streams return yamux:* fallback addresses when the underlying transport
// does not expose net.Addr (like the test pipes used here).
func TestStreamAddrFallback(t *testing.T) {
	c, s := testConnPipe(t)
	client, server := testClientServerConfig(t, c, s, testConfNoKeepAlive(), testConfNoKeepAlive())

	// Open/accept a stream to get a net.Conn
	st, err := client.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func(st net.Conn) {
		_ = st.Close()
	}(st)

	sv, err := server.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer func(sv net.Conn) {
		_ = sv.Close()
	}(sv)

	if got := client.LocalAddr().String(); got != "yamux:local" {
		t.Fatalf("unexpected client LocalAddr: %s", got)
	}
	if got := client.RemoteAddr().String(); got != "yamux:remote" {
		t.Fatalf("unexpected client RemoteAddr: %s", got)
	}

	if got := st.LocalAddr().String(); got != "yamux:local" {
		t.Fatalf("unexpected stream LocalAddr: %s", got)
	}
	if got := st.RemoteAddr().String(); got != "yamux:remote" {
		t.Fatalf("unexpected stream RemoteAddr: %s", got)
	}

	if got := sv.LocalAddr().String(); got != "yamux:local" {
		t.Fatalf("unexpected server stream LocalAddr: %s", got)
	}
	if got := sv.RemoteAddr().String(); got != "yamux:remote" {
		t.Fatalf("unexpected server stream RemoteAddr: %s", got)
	}
}

// TestBacklogOverflowRST configures a small AcceptBacklog and opens more
// streams than allowed without accepting them, expecting some client-side
// streams to get reset (ErrConnectionReset) when attempting to write.
func TestBacklogOverflowRST(t *testing.T) {
	c, s := testConnPipe(t)
	cconf := testConfNoKeepAlive()
	sconf := testConfNoKeepAlive()
	sconf.AcceptBacklog = 1
	client, server := testClientServerConfig(t, c, s, cconf, sconf)
	defer func(server *yamux.Session) {
		_ = server.Close()
	}(server)

	// Open multiple streams quickly.
	const n = 4
	conns := make([]io.ReadWriteCloser, 0, n)
	for i := 0; i < n; i++ {
		st, err := client.Open()
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		conns = append(conns, st)
	}

	// Give the server time to enqueue at most one and reset the rest.
	time.Sleep(50 * time.Millisecond)

	// Attempt writes; at least one should fail with ErrConnectionReset.
	resets := 0
	for i, st := range conns {
		_, err := st.Write([]byte("x"))
		if errors.Is(err, yamux.ErrConnectionReset) {
			resets++
		} else if err != nil {
			t.Logf("stream %d write error: %v", i, err)
		}
		_ = st.Close()
	}
	if resets == 0 {
		t.Fatalf("expected at least one ErrConnectionReset due to backlog overflow")
	}
}
