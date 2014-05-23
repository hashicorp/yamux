package yamux

import (
	"bytes"
	"net"
	"time"
)

type streamState int

const (
	streamSYNSent streamState = iota
	streamSYNReceived
	streamEstablished
	streamLocalClose
	streamRemoteClose
	streamClosed
)

// Stream is used to represent a logical stream
// within a session.
type Stream struct {
	id      uint32
	session *Session

	state streamState

	recvBuf    bytes.Buffer
	recvWindow uint32

	sendBuf    bytes.Buffer
	sendWindow uint32

	readDeadline  time.Time
	writeDeadline time.Time
}

// Session returns the associated stream session
func (s *Stream) Session() *Session {
	return s.session
}

// StreamID returns the ID of this stream
func (s *Stream) StreamID() uint32 {
	return s.id
}

// Read is used to read from the stream
func (s *Stream) Read(b []byte) (int, error) {
	return 0, nil
}

// Write is used to write to the stream
func (s *Stream) Write(b []byte) (int, error) {
	return 0, nil
}

// Close is used to close the stream
func (s *Stream) Close() error {
	return nil
}

// LocalAddr returns the local address
func (s *Stream) LocalAddr() net.Addr {
	return s.session.LocalAddr()
}

// LocalAddr returns the remote address
func (s *Stream) RemoteAddr() net.Addr {
	return s.session.RemoteAddr()
}

// SetDeadline sets the read and write deadlines
func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// SetReadDeadline sets the deadline for future Read calls.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readDeadline = t
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline = t
	return nil
}
