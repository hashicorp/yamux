package yamux_test

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// deadlineRecorderConn wraps a RWC and records SetWriteDeadline calls.
type deadlineRecorderConn struct {
	r io.Reader
	w io.Writer
	c io.Closer

	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineRecorderConn) Read(p []byte) (int, error)  { return d.r.Read(p) }
func (d *deadlineRecorderConn) Write(p []byte) (int, error) { return d.w.Write(p) }
func (d *deadlineRecorderConn) Close() error                { return d.c.Close() }

// Implement the interface used by session.withWriteDeadline.
func (d *deadlineRecorderConn) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	d.deadlines = append(d.deadlines, t)
	d.mu.Unlock()
	return nil
}

func makeDeadlinePair(_ testing.TB) (*deadlineRecorderConn, *deadlineRecorderConn) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	c1 := &deadlineRecorderConn{r: r1, w: w2, c: multiCloser{r1, w2}}
	c2 := &deadlineRecorderConn{r: r2, w: w1, c: multiCloser{r2, w1}}
	return c1, c2
}

type multiCloser struct{ a, b io.Closer }

func (m multiCloser) Close() error { _ = m.a.Close(); return m.b.Close() }

func TestWriteDeadlinesApplied(t *testing.T) {
	clientConn, serverConn := makeDeadlinePair(t)

	cconf := testConf()
	sconf := testConf()
	cconf.ConnectionWriteTimeout = 50 * time.Millisecond
	sconf.ConnectionWriteTimeout = 50 * time.Millisecond

	client, server := testClientServerConfig(t, clientConn, serverConn, cconf, sconf)

	// Write some data client -> server
	s, err := client.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// Wait briefly to allow sendLoop to perform writes
	time.Sleep(50 * time.Millisecond)

	// Check that at least one non-zero deadline was set
	clientConn.mu.Lock()
	defer clientConn.mu.Unlock()

	found := false
	for _, dl := range clientConn.deadlines {
		if !dl.IsZero() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected client SetWriteDeadline to be called with non-zero")
	}

	// Also verify server can echo and set deadlines
	// Accept stream and echo
	go func() {
		st, err := server.Accept()
		if err != nil {
			return
		}
		defer func(st net.Conn) {
			_ = st.Close()
		}(st)
		buf := make([]byte, 5)
		_, _ = io.ReadFull(st, buf)
		_, _ = st.Write([]byte("world"))
	}()

	// Read echo
	buf := make([]byte, 5)
	_, err = io.ReadFull(s, buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}

	serverConn.mu.Lock()
	hasServerDeadline := false
	for _, dl := range serverConn.deadlines {
		if !dl.IsZero() {
			hasServerDeadline = true
			break
		}
	}
	serverConn.mu.Unlock()

	if !hasServerDeadline {
		t.Fatalf("expected server SetWriteDeadline to be called with non-zero")
	}

	_ = client.Close()
	_ = server.Close()
}

// deadlineFailWriteConn simulates a connection whose Write blocks until a
// write deadline is set and reached, then returns a timeout error. This
// allows exercising keepalive and send loop behavior without hanging.
type deadlineFailWriteConn struct {
	r io.Reader
	w io.Writer // unused; writes are blocked by deadline
	c io.Closer

	mu sync.Mutex
	dl time.Time
}

func (d *deadlineFailWriteConn) Read(p []byte) (int, error) { return d.r.Read(p) }

//goland:noinspection GoUnusedParameter
func (d *deadlineFailWriteConn) Write(p []byte) (int, error) {
	for {
		d.mu.Lock()
		dl := d.dl
		d.mu.Unlock()
		if !dl.IsZero() && time.Now().After(dl) {
			return 0, timeoutErr{}
		}
		time.Sleep(1 * time.Millisecond)
	}
}
func (d *deadlineFailWriteConn) Close() error { return d.c.Close() }
func (d *deadlineFailWriteConn) SetWriteDeadline(ti time.Time) error {
	d.mu.Lock()
	d.dl = ti
	d.mu.Unlock()
	return nil
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "write timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// reuse wrapRWC from other tests by defining a local alias
// use wrapRWC defined in accept_cleanup_test.go within the same package

func makeDeadlineFailPair(_ testing.TB) (io.ReadWriteCloser, io.ReadWriteCloser) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	client := &wrapRWC{r: r1, w: w2, c: multiCloser{r1, w2}}
	server := &deadlineFailWriteConn{r: r2, w: w1, c: multiCloser{r2, w1}}
	return client, server
}

func TestKeepAliveTimeoutCloses(t *testing.T) {
	clientConn, serverConn := makeDeadlineFailPair(t)

	cconf := testConf()
	sconf := testConf()
	cconf.EnableKeepAlive = true
	sconf.EnableKeepAlive = true
	cconf.KeepAliveInterval = 25 * time.Millisecond
	sconf.KeepAliveInterval = 25 * time.Millisecond
	// Keep write timeout large to ensure the send path doesn't timeout first.
	cconf.ConnectionWriteTimeout = 1 * time.Second
	sconf.ConnectionWriteTimeout = 1 * time.Second
	// Use a small keepalive timeout to force the closure via missing ACK.
	cconf.KeepAliveTimeout = 25 * time.Millisecond
	sconf.KeepAliveTimeout = 25 * time.Millisecond

	_, server := testClientServerConfig(t, clientConn, serverConn, cconf, sconf)

	// Within a short time, server keepalive Ping should fail due to write timeout
	// and the server session should close itself.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if server.IsClosed() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not close after keepalive timeout")
}
