// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

//go:build go1.25

package yamux

import (
	"net"
	"testing"
	"testing/synctest"
)

// A timer made in one synctest bubble must never reach a session in another.
func TestTimersStayInBubble(t *testing.T) {
	for i := 0; i < 20; i++ {
		synctest.Test(t, func(t *testing.T) {
			c1, c2 := net.Pipe()
			client, err := Client(c1, testConfNoKeepAlive())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server, err := Server(c2, testConfNoKeepAlive())
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()

			cs, err := client.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			ss, err := server.AcceptStream()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cs.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 4)
			if _, err := ss.Read(buf); err != nil {
				t.Fatal(err)
			}
		})
	}
}
