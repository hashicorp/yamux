package yamux

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// gatedConn holds its first write until gate closes and counts writes.
type gatedConn struct {
	net.Conn
	gate   chan struct{}
	writes atomic.Int32
}

func (c *gatedConn) Write(b []byte) (int, error) {
	if c.writes.Add(1) == 1 {
		<-c.gate
	}
	return c.Conn.Write(b)
}

func TestSendLoopBatchesQueuedFrames(t *testing.T) {
	local, remote := net.Pipe()
	got := make(chan int, 1)
	go func() {
		n, _ := io.Copy(io.Discard, remote) //nolint:errcheck
		got <- int(n)
	}()
	conn := &gatedConn{Conn: local, gate: make(chan struct{})}
	conf := DefaultConfig()
	conf.EnableKeepAlive = false
	conf.LogOutput = io.Discard
	s, err := Client(conn, conf)
	if err != nil {
		t.Fatal(err)
	}

	const frames = 11
	for i := 0; i < frames; i++ {
		hdr := header(make([]byte, headerSize))
		hdr.encode(typePing, flagSYN, 0, uint32(i))
		if err := s.sendNoWait(hdr); err != nil {
			t.Fatal(err)
		}
	}
	// The first write is held while the rest queue up behind it.
	time.Sleep(20 * time.Millisecond)
	close(conn.gate)
	for deadline := time.Now().Add(time.Second); len(s.sendCh) > 0; {
		if time.Now().After(deadline) {
			t.Fatal("queue not drained")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	s.Close() //nolint:errcheck

	if w := conn.writes.Load(); w > 2 {
		t.Errorf("%d writes for %d frames, want at most 2", w, frames)
	}
	if n := <-got; n < frames*headerSize {
		t.Errorf("%d bytes written, want at least %d", n, frames*headerSize)
	}
}
