package yamux

import (
	"io"
	"testing"
	"time"
)

// A data frame that also carries FIN must deliver its payload before Read
// reports EOF, even when a reader is already blocked on the stream.
func TestStream_DataFrameWithFINDeliversPayload(t *testing.T) {
	raw, conn := testConnPipe(t)
	server, err := Server(conn, testConf())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go io.Copy(io.Discard, raw)

	payload := []byte("hello")
	hdr := header(make([]byte, headerSize))
	for id := uint32(1); id < 2000; id += 2 {
		hdr.encode(typeWindowUpdate, flagSYN, id, 0)
		if _, err := raw.Write(hdr); err != nil {
			t.Fatal(err)
		}
		stream, err := server.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan []byte, 1)
		// Poll with an expired deadline so a Read is in flight when the
		// frame lands, which is when a reader can see FIN before the data.
		if err := stream.SetReadDeadline(time.Now()); err != nil {
			t.Fatal(err)
		}
		go func() {
			var buf []byte
			b := make([]byte, 16)
			for {
				n, err := stream.Read(b)
				buf = append(buf, b[:n]...)
				if err == ErrTimeout {
					continue
				}
				got <- buf
				return
			}
		}()

		frame := append(make([]byte, 0, headerSize+len(payload)), hdr...)
		header(frame[:headerSize]).encode(typeData, flagFIN, id, uint32(len(payload)))
		frame = append(frame[:headerSize], payload...)
		if _, err := raw.Write(frame); err != nil {
			t.Fatal(err)
		}

		select {
		case buf := <-got:
			if string(buf) != string(payload) {
				t.Fatalf("stream %d: read %q, want %q", id, buf, payload)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("stream %d: timed out waiting for read", id)
		}
		stream.Close()
	}
}
