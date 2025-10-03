package yamux_test

import (
	"testing"

	yamux "github.com/hashicorp/yamux"
)

func TestVerifyConfig_KeepAliveDisabled_OK(t *testing.T) {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = false
	c.KeepAliveInterval = 0
	if err := yamux.VerifyConfig(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyConfig_KeepAliveEnabled_Invalid(t *testing.T) {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 0
	if err := yamux.VerifyConfig(c); err == nil {
		t.Fatalf("expected error when keepalive enabled with zero interval")
	}
}
