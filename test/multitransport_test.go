package yamux_test

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestMultiTransport_Basic exercises basic open/accept, echo, and ping across
// multiple underlying connection types (Pipe, TCP, TLS).
func TestMultiTransport_Basic(t *testing.T) {
	transports := []struct {
		Name  string
		Conns func(testing.TB) (io.ReadWriteCloser, io.ReadWriteCloser)
	}{
		{Name: "Pipe", Conns: testConnPipe},
		{Name: "TCP", Conns: testConnTCP},
		{Name: "TLS", Conns: testConnTLS},
	}

	for _, tr := range transports {
		t.Run(tr.Name, func(t *testing.T) {
			c, s := tr.Conns(t)
			cconf := testConfNoKeepAlive()
			sconf := testConfNoKeepAlive()
			client, server := testClientServerConfig(t, c, s, cconf, sconf)

			// Ping both ways
			if _, err := client.Ping(); err != nil {
				t.Fatalf("client ping: %v", err)
			}
			if _, err := server.Ping(); err != nil {
				t.Fatalf("server ping: %v", err)
			}

			// Echo a small payload
			payload := []byte("hello over " + tr.Name)

			done := make(chan struct{})
			go func() {
				st, err := server.Accept()
				if err != nil {
					close(done)
					return
				}
				defer func(st net.Conn) {
					_ = st.Close()
				}(st)
				buf := make([]byte, len(payload))
				_, _ = io.ReadFull(st, buf)
				_, _ = st.Write(buf)
				close(done)
			}()

			st, err := client.Open()
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if _, err := st.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(st, buf); err != nil {
				t.Fatalf("read echo: %v", err)
			}
			_ = st.Close()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("timeout waiting for server echo on %s", tr.Name)
			}
		})
	}
}
