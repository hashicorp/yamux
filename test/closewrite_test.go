package yamux_test

import (
	"errors"
	"io"
	"testing"

	yamux "github.com/hashicorp/yamux"
)

type closeWriter interface{ CloseWrite() error }

func TestCloseWrite_HalfClose(t *testing.T) {
	client, server := testClientServer(t)

	// Open stream client->server
	s, err := client.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Server accepts and will echo data back
	acceptCh := make(chan io.ReadWriteCloser, 1)
	go func() {
		st, err := server.Accept()
		if err != nil {
			return
		}
		acceptCh <- st
	}()

	st := <-acceptCh
	t.Cleanup(func() { _ = st.Close() })

	// Close local write side
	cw, ok := s.(closeWriter)
	if !ok {
		t.Fatalf("stream does not implement CloseWrite")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("closewrite: %v", err)
	}

	// Server can still write to client
	if _, err := st.Write([]byte("hello")); err != nil {
		t.Fatalf("server write: %v", err)
	}

	buf := make([]byte, 5)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatalf("client read: %v", err)
	}

	// Client writes should now fail with ErrStreamClosed
	if _, err := s.Write([]byte("x")); !errors.Is(err, yamux.ErrStreamClosed) {
		t.Fatalf("expected ErrStreamClosed on write after CloseWrite, got: %v", err)
	}
}
