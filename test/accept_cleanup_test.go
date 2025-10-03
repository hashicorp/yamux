package yamux_test

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

var errFailWrite = errors.New("forced write failure")

// failingWriteConn is a RWC whose Write always fails. Reads work via the provided reader.
type failingWriteConn struct {
	r io.Reader
	c io.Closer
}

func (f *failingWriteConn) Read(p []byte) (int, error) { return f.r.Read(p) }

//goland:noinspection GoUnusedParameter
func (f *failingWriteConn) Write(p []byte) (int, error) { return 0, errFailWrite }
func (f *failingWriteConn) Close() error                { return f.c.Close() }

// Also implement SetWriteDeadline to satisfy writeDeadliner if asserted.
//
//goland:noinspection GoUnusedParameter
func (f *failingWriteConn) SetWriteDeadline(time.Time) error { return nil }

// paired with a normal pipe side
func makeFailingPair(_ testing.TB) (io.ReadWriteCloser, io.ReadWriteCloser) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	// client side: normal (reads r1, writes w2)
	client := struct {
		io.Reader
		io.Writer
		io.Closer
	}{Reader: r1, Writer: w2, Closer: multiCloser{r1, w2}}
	// server side: reads r2, but writes always fail
	server := &failingWriteConn{r: r2, c: multiCloser{r2, w1}}
	// Wrap client to expose ReadWriteCloser
	clientRWC := &wrapRWC{r: client.Reader, w: client.Writer, c: client.Closer}
	return clientRWC, server
}

type wrapRWC struct {
	r io.Reader
	w io.Writer
	c io.Closer
}

func (w *wrapRWC) Read(p []byte) (int, error)  { return w.r.Read(p) }
func (w *wrapRWC) Write(p []byte) (int, error) { return w.w.Write(p) }
func (w *wrapRWC) Close() error                { return w.c.Close() }

func TestAcceptStream_AckFailureCleansUp(t *testing.T) {
	clientConn, serverConn := makeFailingPair(t)
	cconf := testConf()
	sconf := testConf()
	// Ensure we fail fast on send
	cconf.ConnectionWriteTimeout = 50 * time.Millisecond
	sconf.ConnectionWriteTimeout = 50 * time.Millisecond

	client, server := testClientServerConfig(t, clientConn, serverConn, cconf, sconf)

	errCh := make(chan error, 1)
	go func() {
		_, err := server.AcceptStream()
		errCh <- err
	}()

	// Trigger an inbound SYN by opening a client stream
	st, err := client.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func(st net.Conn) {
		_ = st.Close()
	}(st)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected error from AcceptStream due to write failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for AcceptStream error")
	}

	// After failure, session should be closing, and streams should be cleaned.
	// Allow a brief grace period for cleanup to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if server.IsClosed() || server.NumStreams() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server still open or streams not cleaned: closed=%v streams=%d", server.IsClosed(), server.NumStreams())
}
