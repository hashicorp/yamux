package yamux

import (
	"io"
	"sync"
	"testing"
	"time"
)

type pipeConn struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func (p *pipeConn) Read(b []byte) (int, error) {
	return p.reader.Read(b)
}

func (p *pipeConn) Write(b []byte) (int, error) {
	return p.writer.Write(b)
}

func (p *pipeConn) Close() error {
	p.reader.Close()
	return p.writer.Close()
}

func testConn() (io.ReadWriteCloser, io.ReadWriteCloser) {
	read1, write1 := io.Pipe()
	read2, write2 := io.Pipe()
	return &pipeConn{read1, write2}, &pipeConn{read2, write1}
}

func testClientServer() (*Session, *Session) {
	conn1, conn2 := testConn()
	client, _ := Client(conn1, nil)
	server, _ := Server(conn2, nil)
	return client, server
}

func TestPing(t *testing.T) {
	client, server := testClientServer()
	defer client.Close()
	defer server.Close()

	rtt, err := client.Ping()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rtt == 0 {
		t.Fatalf("bad: %v", rtt)
	}

	rtt, err = server.Ping()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rtt == 0 {
		t.Fatalf("bad: %v", rtt)
	}
}

func TestAccept(t *testing.T) {
	client, server := testClientServer()
	defer client.Close()
	defer server.Close()

	wg := &sync.WaitGroup{}
	wg.Add(4)

	go func() {
		defer wg.Done()
		stream, err := server.AcceptStream()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if id := stream.StreamID(); id != 1 {
			t.Fatalf("bad: %v", id)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		stream, err := client.AcceptStream()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if id := stream.StreamID(); id != 2 {
			t.Fatalf("bad: %v", id)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		stream, err := server.Open()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if id := stream.StreamID(); id != 2 {
			t.Fatalf("bad: %v", id)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		stream, err := client.Open()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if id := stream.StreamID(); id != 1 {
			t.Fatalf("bad: %v", id)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(time.Second):
		panic("timeout")
	}
}

func TestSendData_Small(t *testing.T) {
	client, server := testClientServer()
	defer client.Close()
	defer server.Close()

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		stream, err := server.AcceptStream()
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		buf := make([]byte, 4)
		for i := 0; i < 1000; i++ {
			n, err := stream.Read(buf)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if n != 4 {
				t.Fatalf("short read: %d", n)
			}
			if string(buf) != "test" {
				t.Fatalf("bad: %s", buf)
			}
		}

		if err := stream.Close(); err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		stream, err := client.Open()
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		for i := 0; i < 1000; i++ {
			n, err := stream.Write([]byte("test"))
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if n != 4 {
				t.Fatalf("short write %d", n)
			}
		}

		if err := stream.Close(); err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		panic("timeout")
	}
}

func TestSendData_Large(t *testing.T) {
	client, server := testClientServer()
	defer client.Close()
	defer server.Close()

	data := make([]byte, 512*1024)
	for idx := range data {
		data[idx] = byte(idx % 256)
	}

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		stream, err := server.AcceptStream()
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		buf := make([]byte, 4*1024)
		for i := 0; i < 128; i++ {
			n, err := stream.Read(buf)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if n != 4*1024 {
				t.Fatalf("short read: %d", n)
			}
			for idx := range buf {
				if buf[idx] != byte(idx%256) {
					t.Fatalf("bad: %v %v %v", i, idx, buf[idx])
				}
			}
		}

		if err := stream.Close(); err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		stream, err := client.Open()
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		n, err := stream.Write(data)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if n != len(data) {
			t.Fatalf("short write %d", n)
		}

		if err := stream.Close(); err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		panic("timeout")
	}
}

func TestGoAway(t *testing.T) {
	client, server := testClientServer()
	defer client.Close()
	defer server.Close()

	if err := server.GoAway(); err != nil {
		t.Fatalf("err: %v", err)
	}

	_, err := client.Open()
	if err != ErrRemoteGoAway {
		t.Fatalf("err: %v", err)
	}
}
