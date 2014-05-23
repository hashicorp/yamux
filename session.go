package yamux

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

var (
	// ErrInvalidVersion means we received a frame with an
	// invalid version
	ErrInvalidVersion = fmt.Errorf("invalid protocol version")

	// ErrInvalidMsgType means we received a frame with an
	// invalid message type
	ErrInvalidMsgType = fmt.Errorf("invalid msg type")

	// ErrSessionShutdown is used if there is a shutdown during
	// an operation
	ErrSessionShutdown = fmt.Errorf("session shutdown")
)

// Session is used to wrap a reliable ordered connection and to
// multiplex it into multiple streams.
type Session struct {
	// client is true if we are a client size connection
	client bool

	// config holds our configuration
	config *Config

	// conn is the underlying connection
	conn io.ReadWriteCloser

	// nextStreamID is the next stream we should
	// send. This depends if we are a client/server.
	nextStreamID uint32

	// pings is used to track inflight pings
	pings    map[uint32]chan struct{}
	pingID   uint32
	pingLock sync.Mutex

	// streams maps a stream id to a stream
	streams map[uint32]*Stream

	// acceptCh is used to pass ready streams to the client
	acceptCh chan *Stream

	// sendCh is used to mark a stream as ready to send,
	// or to send a header out directly.
	sendCh chan sendReady

	// shutdown is used to safely close a session
	shutdown     bool
	shutdownErr  error
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex
}

// hasAddr is used to get the address from the underlying connection
type hasAddr interface {
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

// yamuxAddr is used when we cannot get the underlying address
type yamuxAddr struct {
	Addr string
}

func (*yamuxAddr) Network() string {
	return "yamux"
}

func (y *yamuxAddr) String() string {
	return fmt.Sprintf("yamux:%s", y.Addr)
}

// sendReady is used to either mark a stream as ready
// or to directly send a header
type sendReady struct {
	StreamID uint32
	Hdr      []byte
	Err      chan error
}

// newSession is used to construct a new session
func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := &Session{
		client:     client,
		config:     config,
		conn:       conn,
		pings:      make(map[uint32]chan struct{}),
		streams:    make(map[uint32]*Stream),
		acceptCh:   make(chan *Stream, config.AcceptBacklog),
		sendCh:     make(chan sendReady, 64),
		shutdownCh: make(chan struct{}),
	}
	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 2
	}
	go s.recv()
	go s.send()
	if config.EnableKeepAlive {
		go s.keepalive()
	}
	return s
}

// Open is used to create a new stream
func (s *Session) Open() (*Stream, error) {
	return nil, nil
}

// Accept is used to block until the next available stream
// is ready to be accepted.
func (s *Session) Accept() (net.Conn, error) {
	return s.AcceptStream()
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	select {
	case stream := <-s.acceptCh:
		return stream, nil
	case <-s.shutdownCh:
		return nil, s.shutdownErr
	}
}

// Close is used to close the session and all streams.
// Attempts to send a GoAway before closing the connection.
func (s *Session) Close() error {
	s.shutdownLock.Lock()
	defer s.shutdownLock.Unlock()

	if s.shutdown {
		return nil
	}
	s.shutdown = true
	close(s.shutdownCh)
	s.conn.Close()
	return nil
}

// Addr is used to get the address of the listener.
func (s *Session) Addr() net.Addr {
	return s.LocalAddr()
}

// LocalAddr is used to get the local address of the
// underlying connection.
func (s *Session) LocalAddr() net.Addr {
	addr, ok := s.conn.(hasAddr)
	if !ok {
		return &yamuxAddr{"local"}
	}
	return addr.LocalAddr()
}

// RemoteAddr is used to get the address of remote end
// of the underlying connection
func (s *Session) RemoteAddr() net.Addr {
	addr, ok := s.conn.(hasAddr)
	if !ok {
		return &yamuxAddr{"remote"}
	}
	return addr.RemoteAddr()
}

// Ping is used to measure the RTT response time
func (s *Session) Ping() (time.Duration, error) {
	// Get a channel for the ping
	ch := make(chan struct{})

	// Get a new ping id, mark as pending
	s.pingLock.Lock()
	id := s.pingID
	s.pingID++
	s.pings[id] = ch
	s.pingLock.Unlock()

	// Send the ping request
	hdr := header(make([]byte, headerSize))
	hdr.encode(typePing, flagSYN, 0, id)
	if err := s.waitForSend(hdr); err != nil {
		return 0, err
	}

	// Wait for a response
	start := time.Now()
	select {
	case <-ch:
	case <-s.shutdownCh:
		return 0, ErrSessionShutdown
	}

	// Compute the RTT
	return time.Now().Sub(start), nil
}

// keepalive is a long running goroutine that periodically does
// a ping to keep the connection alive.
func (s *Session) keepalive() {
	for {
		select {
		case <-time.After(s.config.KeepAliveInterval):
			s.Ping()
		case <-s.shutdownCh:
			return
		}
	}
}

// waitForSend waits to send a header, checking for a potential shutdown
func (s *Session) waitForSend(hdr header) error {
	errCh := make(chan error, 1)
	ready := sendReady{Hdr: hdr, Err: errCh}
	select {
	case s.sendCh <- ready:
	case <-s.shutdownCh:
		return ErrSessionShutdown
	}
	select {
	case err := <-errCh:
		return err
	case <-s.shutdownCh:
		return ErrSessionShutdown
	}
}

// sendNoWait does a send without waiting
func (s *Session) sendNoWait(hdr header) error {
	select {
	case s.sendCh <- sendReady{Hdr: hdr}:
		return nil
	case <-s.shutdownCh:
		return ErrSessionShutdown
	}
}

// send is a long running goroutine that sends data
func (s *Session) send() {
	for {
		select {
		case ready := <-s.sendCh:
			// Send data from a stream if ready
			if ready.StreamID != 0 {

			}

			// Send a header if ready
			if ready.Hdr != nil {
				sent := 0
				for sent < len(ready.Hdr) {
					n, err := s.conn.Write(ready.Hdr[sent:])
					if err != nil {
						s.exitErr(err)
						asyncSendErr(ready.Err, err)
					}
					sent += n
				}
			}
			asyncSendErr(ready.Err, nil)
		case <-s.shutdownCh:
			return
		}
	}
}

// recv is a long running goroutine that accepts new data
func (s *Session) recv() {
	hdr := header(make([]byte, headerSize))
	for {
		// Read the header
		if _, err := io.ReadFull(s.conn, hdr); err != nil {
			s.exitErr(err)
			return
		}

		// Verify the version
		if hdr.Version() != protoVersion {
			s.exitErr(ErrInvalidVersion)
			return
		}

		// Switch on the type
		msgType := hdr.MsgType()
		switch msgType {
		case typeData:
			s.handleData(hdr)
		case typeWindowUpdate:
			s.handleWindowUpdate(hdr)
		case typePing:
			s.handlePing(hdr)
		case typeGoAway:
			s.handleGoAway(hdr)
		default:
			s.exitErr(ErrInvalidMsgType)
			return
		}
	}
}

// handleData is invokde for a typeData frame
func (s *Session) handleData(hdr header) {
	flags := hdr.Flags()

	// Check for a new stream creation
	if flags&flagSYN == flagSYN {
		s.createStream(hdr.StreamID())
	}
}

// handleWindowUpdate is invokde for a typeWindowUpdate frame
func (s *Session) handleWindowUpdate(hdr header) {
	flags := hdr.Flags()

	// Check for a new stream creation
	if flags&flagSYN == flagSYN {
		s.createStream(hdr.StreamID())
	}
}

// handlePing is invokde for a typePing frame
func (s *Session) handlePing(hdr header) {
	flags := hdr.Flags()
	pingID := hdr.Length()

	// Check if this is a query, respond back
	if flags&flagSYN == flagSYN {
		hdr := header(make([]byte, headerSize))
		hdr.encode(typePing, flagACK, 0, pingID)
		s.sendNoWait(hdr)
		return
	}

	// Handle a response
	s.pingLock.Lock()
	ch := s.pings[pingID]
	if ch != nil {
		delete(s.pings, pingID)
		close(ch)
	}
	s.pingLock.Unlock()
}

// handleGoAway is invokde for a typeGoAway frame
func (s *Session) handleGoAway(hdr header) {

}

// exitErr is used to handle an error that is causing
// the listener to exit.
func (s *Session) exitErr(err error) {
}

// goAway is used to send a goAway message
func (s *Session) goAway(reason uint32) {
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeGoAway, 0, 0, reason)
	s.sendNoWait(hdr)
}

// createStream is used to create a new stream
func (s *Session) createStream(id uint32) {
	// TODO
}
