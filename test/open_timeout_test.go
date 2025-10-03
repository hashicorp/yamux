package yamux_test

import (
	"strings"
	"testing"
	"time"
)

// TestOpenStreamTimeout_LogsAndCloses verifies that when a stream remains
// unacknowledged beyond StreamOpenTimeout, the session logs an abort message
// and closes.
func TestOpenStreamTimeout_LogsAndCloses(t *testing.T) {
	const timeout = 25 * time.Millisecond

	clientConn, serverConn := testConnPipe(t)

	serverConf := testConfNoKeepAlive()
	clientConf := testConfNoKeepAlive()
	clientConf.StreamOpenTimeout = timeout
	logs := captureLogs(clientConf)

	client, _ := testClientServerConfig(t, clientConn, serverConn, clientConf, serverConf)

	// Open a stream; do not Accept on server so no ACK is sent.
	st, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	// Keep a reference to avoid GC.
	_ = st

	// Wait a bit longer than the timeout for the abort to trigger.
	time.Sleep(timeout * 5)

	if !client.IsClosed() {
		t.Fatalf("client session should be closed after open timeout")
	}

	// Verify we logged the abort line (substr match to be robust across transports)
	if !strings.Contains(logs.String(), "aborted stream open") {
		t.Fatalf("expected abort log; got: %s", logs.String())
	}
}
