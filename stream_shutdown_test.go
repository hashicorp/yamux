// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// testSessionPair wires a client and server Session over an in-memory pipe.
func testSessionPair(t *testing.T) (client, server *Session) {
	t.Helper()
	cConn, sConn := net.Pipe()

	cfg := DefaultConfig()
	cfg.EnableKeepAlive = false // keep the test deterministic (no background pings)

	var err error
	client, err = Client(cConn, cfg.Clone())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	server, err = Server(sConn, cfg.Clone())
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// TestStreamReadReturnsOnSessionShutdown asserts that a Read blocked with no
// data available returns promptly once the session is closed, rather than
// spinning. This is the base guarantee restored by #3924.
func TestStreamReadReturnsOnSessionShutdown(t *testing.T) {
	client, server := testSessionPair(t)

	// Establish a stream so both ends have it in streamEstablished.
	go func() {
		s, err := server.AcceptStream()
		if err == nil {
			// Read once so the server side is parked too; ignore result.
			buf := make([]byte, 1)
			_, _ = s.Read(buf)
		}
	}()

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, rerr := stream.Read(buf)
		done <- rerr
	}()

	// Give the reader a moment to park in WAIT, then tear the session down.
	time.Sleep(50 * time.Millisecond)
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case rerr := <-done:
		if rerr == nil {
			t.Fatal("expected an error from Read after session shutdown, got nil")
		}
		if rerr != ErrSessionShutdown && rerr != io.EOF {
			t.Fatalf("unexpected error: %v (want ErrSessionShutdown or io.EOF)", rerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return within 2s after session shutdown (spin/hang)")
	}
}

// TestStreamReadDoesNotSpinOnNotifyAfterShutdown reproduces the residual spin
// directly and deterministically. It puts a stream in streamEstablished, marks
// the session shut down, and then hammers recvNotifyCh so the WAIT select's
// notify case is (essentially) always ready. Without the non-blocking
// shutdownCh pre-check, the retry loop makes no forward progress and — with no
// read deadline set — allocates nothing, matching the observed js/wasm freeze
// (num_gc unchanged, non-preemptible). With the fix, Read returns promptly no
// matter how aggressively recvNotifyCh is signaled.
func TestStreamReadDoesNotSpinOnNotifyAfterShutdown(t *testing.T) {
	client, _ := testSessionPair(t)

	stream := newStream(client, 1, streamEstablished)

	// Mark the session as shut down the same way Session.Close() does, but
	// WITHOUT forceClosing the stream, so its state stays streamEstablished —
	// exactly the race window Close() opens between close(shutdownCh) and
	// forceClose(streams).
	client.shutdownLock.Lock()
	if !client.shutdown {
		client.shutdown = true
		client.shutdownErr = ErrSessionShutdown
		close(client.shutdownCh)
	}
	client.shutdownLock.Unlock()

	// Continuously re-signal recvNotifyCh to keep the notify case ready.
	var stop int32
	go func() {
		for atomic.LoadInt32(&stop) == 0 {
			asyncNotify(stream.recvNotifyCh)
		}
	}()
	defer atomic.StoreInt32(&stop, 1)

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, rerr := stream.Read(buf)
		done <- rerr
	}()

	select {
	case rerr := <-done:
		if rerr != ErrSessionShutdown {
			t.Fatalf("expected ErrSessionShutdown, got %v", rerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read spun on recvNotifyCh instead of returning on shutdown")
	}
}

// TestStreamWriteDoesNotSpinOnNotifyAfterShutdown is the write-side analogue:
// a full send window plus a shut-down session must fail the write promptly even
// while sendNotifyCh is continuously signaled.
func TestStreamWriteDoesNotSpinOnNotifyAfterShutdown(t *testing.T) {
	client, _ := testSessionPair(t)

	stream := newStream(client, 1, streamEstablished)
	// Exhaust the send window so write() parks in WAIT.
	atomic.StoreUint32(&stream.sendWindow, 0)

	client.shutdownLock.Lock()
	if !client.shutdown {
		client.shutdown = true
		client.shutdownErr = ErrSessionShutdown
		close(client.shutdownCh)
	}
	client.shutdownLock.Unlock()

	var stop int32
	go func() {
		for atomic.LoadInt32(&stop) == 0 {
			asyncNotify(stream.sendNotifyCh)
		}
	}()
	defer atomic.StoreInt32(&stop, 1)

	done := make(chan error, 1)
	go func() {
		_, werr := stream.Write([]byte("payload"))
		done <- werr
	}()

	select {
	case werr := <-done:
		if werr != ErrSessionShutdown {
			t.Fatalf("expected ErrSessionShutdown, got %v", werr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write spun on sendNotifyCh instead of returning on shutdown")
	}
}
