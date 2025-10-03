package yamux_test

import (
	"context"
	"errors"
	"testing"
	"time"

	yamux "github.com/hashicorp/yamux"
)

func TestAccept_ReturnsRemoteGoAway(t *testing.T) {
	// Use pipes to avoid TLS overhead
	cConn, sConn := testConnPipe(t)
	cConf := testConf()
	sConf := testConf()

	client, server := testClientServerConfig(t, cConn, sConn, cConf, sConf)

	// Send go-away from client to server side
	if err := client.GoAway(); err != nil {
		t.Fatalf("goaway: %v", err)
	}

	// Server Accept should return quickly with ErrRemoteGoAway
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := server.AcceptStreamWithContext(ctx)
	if !errors.Is(err, yamux.ErrRemoteGoAway) {
		t.Fatalf("expected ErrRemoteGoAway, got: %v", err)
	}
}
