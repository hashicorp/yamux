package yamux_test

import (
	"testing"
)

func TestPing_Basic(t *testing.T) {
	client, server := testClientServer(t)

	rtt, err := client.Ping()
	if err != nil {
		t.Fatalf("client ping failed: rtt=%v err=%v", rtt, err)
	}

	rtt, err = server.Ping()
	if err != nil {
		t.Fatalf("server ping failed: rtt=%v err=%v", rtt, err)
	}
}
