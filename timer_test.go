// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"errors"
	"io"
	"testing"
	"time"
)

func TestDeadlineTimerReuse(t *testing.T) {
	client, server := testClientServer(t)

	cs, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	ss, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	for i := 0; i < 5; i++ {
		if err := ss.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if _, err := ss.Read(buf); !errors.Is(err, ErrTimeout) {
			t.Fatalf("round %d: want timeout, got %v", i, err)
		}
		if err := ss.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if n, err := ss.Read(buf); n != 1 || err != nil {
			t.Fatalf("round %d: read %d %v", i, n, err)
		}
	}
}

func TestDeadlineTimerAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping allocation count, which the race detector inflates")
	}
	c1, c2 := testConnPipe(t)
	client, server := testClientServerConfig(t, c1, c2, testConfNoKeepAlive(), testConfNoKeepAlive())
	cs, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.AcceptStream(); err != nil {
		t.Fatal(err)
	}

	// An expired deadline on an empty stream takes the timer path once per Read.
	if err := cs.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := cs.Read(buf); !errors.Is(err, ErrTimeout) {
			t.Fatalf("want timeout, got %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("want no allocs per timed out Read, got %v", allocs)
	}
}

func BenchmarkDeadlineEcho(b *testing.B) {
	client, server := testClientServer(b)
	cs, err := client.OpenStream()
	if err != nil {
		b.Fatal(err)
	}
	ss, err := server.AcceptStream()
	if err != nil {
		b.Fatal(err)
	}
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := ss.Read(buf)
			if err != nil {
				return
			}
			if _, err := ss.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
	if err := cs.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		b.Fatal(err)
	}
	p := make([]byte, 64)
	buf := make([]byte, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := cs.Write(p); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(cs, buf); err != nil {
			b.Fatal(err)
		}
	}
}
