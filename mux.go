package yamux

import (
	"io"
	"time"
)

// Config is used to tune the Yamux session
type Config struct {
	// AcceptBacklog is used to limit how many streams may be
	// waiting an accept.
	AcceptBacklog int

	// EnableCompression is used to control if we compress
	// outgoing data. We have no control over incoming data.
	EnableCompression bool

	// EnableKeepalive is used to do a period keep alive
	// messages using a ping.
	EnableKeepAlive bool

	// KeepAliveInterval is how often to perform the keep alive
	KeepAliveInterval time.Duration

	// MaxSessionWindowSize is used to control the maximum
	// window size that we allow for a session.
	MaxSessionWindowSize uint32

	// MaxStreamWindowSize is used to control the maximum
	// window size that we allow for a stream.
	MaxStreamWindowSize uint32
}

// DefaultConfig is used to return a default configuration
func DefaultConfig() *Config {
	return &Config{
		AcceptBacklog:        256,
		EnableCompression:    true,
		EnableKeepAlive:      true,
		KeepAliveInterval:    30 * time.Second,
		MaxSessionWindowSize: initialSessionWindow,
		MaxStreamWindowSize:  initialStreamWindow,
	}
}

// Server is used to initialize a new server-side connection.
// There must be at most one server-side connection. If a nil config is
// provided, the DefaultConfiguration will be used.
func Server(conn io.ReadWriteCloser, config *Config) *Session {
	if config == nil {
		config = DefaultConfig()
	}
	return newSession(config, conn, false)
}

// Client is used to initialize a new client-side connection.
// There must be at most one client-side connection.
func Client(conn io.ReadWriteCloser, config *Config) *Session {
	if config == nil {
		config = DefaultConfig()
	}
	return newSession(config, conn, true)
}
