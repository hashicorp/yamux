package yamux_test

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestStress_ManyStreamsAndPayloads opens a number of streams concurrently and
// transfers moderate payloads to validate stability under concurrency without
// taking too long. It uses pipe connections to avoid TLS overhead.
func TestStress_ManyStreamsAndPayloads(t *testing.T) {
	const (
		streams    = 32
		payloadLen = 8 << 10 // 8 KiB
	)

	c, s := testConnPipe(t)
	cconf := testConfNoKeepAlive()
	sconf := testConfNoKeepAlive()
	client, server := testClientServerConfig(t, c, s, cconf, sconf)

	// Server accept loop
	var serverWG sync.WaitGroup
	serverWG.Add(1)
	go func() {
		defer serverWG.Done()
		buf := make([]byte, payloadLen)
		for i := 0; i < streams; i++ {
			st, err := server.Accept()
			if err != nil {
				return
			}
			_, _ = io.ReadFull(st, buf)
			_ = st.Close()
		}
	}()

	// Client writers
	data := make([]byte, payloadLen)
	var wg sync.WaitGroup
	wg.Add(streams)
	for i := 0; i < streams; i++ {
		go func() {
			defer wg.Done()
			st, err := client.Open()
			if err != nil {
				t.Errorf("open: %v", err)
				return
			}
			defer func(st net.Conn) {
				_ = st.Close()
			}(st)
			if _, err := st.Write(data); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for client writers")
	}

	serverWG.Wait()
}

// BenchmarkThroughputSmallStream opens a fresh stream per iteration and writes
// a small payload. This stresses stream open/close and header coalescing.
func BenchmarkThroughputSmallStream(b *testing.B) {
	c, s := testConnPipe(b)
	cconf := testConfNoKeepAlive()
	sconf := testConfNoKeepAlive()
	client, server := testClientServerConfig(b, c, s, cconf, sconf)

	const payloadLen = 16 << 10 // 16 KiB
	payload := make([]byte, payloadLen)

	// Server loop to accept and drain exactly b.N streams.
	done := make(chan struct{})
	go func() {
		buf := make([]byte, payloadLen)
		for i := 0; i < b.N; i++ {
			st, err := server.Accept()
			if err != nil {
				break
			}
			_, _ = io.ReadFull(st, buf)
			_ = st.Close()
		}
		close(done)
	}()

	b.SetBytes(int64(payloadLen))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := client.Open()
		if err != nil {
			b.Fatalf("open: %v", err)
		}
		if _, err := st.Write(payload); err != nil {
			b.Fatalf("write: %v", err)
		}
		_ = st.Close()
	}
	<-done
}
