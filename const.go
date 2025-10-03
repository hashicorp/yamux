package yamux

import (
	"encoding/binary"
	"fmt"
)

// NetError implements the net.Error interface and is used to signal
// network-style conditions (e.g., timeouts). The timeout and temporary
// flags control caller behavior such as retries and backoff.
type NetError struct {
	err       error
	timeout   bool
	temporary bool
}

func (e *NetError) Error() string {
	return e.err.Error()
}

func (e *NetError) Timeout() bool {
	return e.timeout
}

func (e *NetError) Temporary() bool {
	return e.temporary
}

var (
	// ErrInvalidVersion indicates a frame with an unsupported protocol version
	// was received.
	ErrInvalidVersion = fmt.Errorf("invalid protocol version")

	// ErrInvalidMsgType indicates a frame carried an unknown/invalid
	// message type.
	ErrInvalidMsgType = fmt.Errorf("invalid msg type")

	// ErrSessionShutdown is returned if the session is shutting down or is
	// already closed while an operation is in flight.
	ErrSessionShutdown = fmt.Errorf("session shutdown")

	// ErrStreamsExhausted indicates there are no more stream IDs to issue for
	// the current side (client/server).
	ErrStreamsExhausted = fmt.Errorf("streams exhausted")

	// ErrDuplicateStream indicates a duplicate inbound stream was declared.
	ErrDuplicateStream = fmt.Errorf("duplicate stream initiated")

	// ErrRecvWindowExceeded indicates the advertised receive window was exceeded.
	ErrRecvWindowExceeded = fmt.Errorf("recv window exceeded")

	// ErrTimeout is returned when an I/O deadline is reached. In the context of
	// Yamux flow control and backpressure, this can indicate that a write or
	// read could not make forward progress within the configured deadline.
	// Callers may interpret ErrTimeout as a signal of backpressure and
	// implement retries or adaptive throttling as appropriate.
	ErrTimeout = &NetError{
		err: fmt.Errorf("i/o deadline reached"),

		// Error should meet net.Error interface for timeouts for compatibility
		// with standard library expectations, such as http servers.
		timeout: true,
	}

	// ErrStreamClosed indicates an operation attempted on a closed stream.
	ErrStreamClosed = fmt.Errorf("stream closed")

	// ErrUnexpectedFlag indicates a frame carried an unexpected flag for the
	// current stream state.
	ErrUnexpectedFlag = fmt.Errorf("unexpected flag")

	// ErrRemoteGoAway indicates the remote side sent a GoAway and is not
	// accepting further streams.
	ErrRemoteGoAway = fmt.Errorf("remote end is not accepting connections")

	// ErrConnectionReset indicates a stream was reset. This can happen if the
	// backlog is exceeded or after a remote GoAway.
	ErrConnectionReset = fmt.Errorf("connection reset")

	// ErrConnectionWriteTimeout indicates the per-write "safety valve"
	// deadline was hit on the underlying connection.
	ErrConnectionWriteTimeout = fmt.Errorf("connection write timeout")

	// ErrKeepAliveTimeout indicates a missed keep-alive caused the session to
	// close.
	ErrKeepAliveTimeout = fmt.Errorf("keepalive timeout")
)

const (
	// protoVersion is the only protocol version supported by this library.
	protoVersion uint8 = 0
)

const (
	// typeData marks a data frame followed by Length bytes of payload.
	typeData uint8 = iota

	// typeWindowUpdate updates the receiver's window for a given stream. The
	// Length field carries the delta.
	typeWindowUpdate

	// typePing is used for RTT measurement and keep-alive. The opaque Length
	// value is echoed back in the response.
	typePing

	// typeGoAway terminates a session. StreamID must be 0 and Length carries
	// an error code.
	typeGoAway
)

const (
	// flagSYN signals the start of a new stream. May be sent with a data or
	// window update payload.
	flagSYN uint16 = 1 << iota

	// flagACK acknowledges the start of a new stream. May be sent with a data
	// or window update payload.
	flagACK

	// flagFIN performs a half-close of the given stream. May be sent with a
	// data or window update payload.
	flagFIN

	// flagRST performs an immediate hard close of the given stream.
	flagRST
)

const (
	// initialStreamWindow is the initial stream window size
	initialStreamWindow uint32 = 256 * 1024
)

const (
	// goAwayNormal indicates a normal session termination.
	goAwayNormal uint32 = iota

	// goAwayProtoErr indicates a protocol error.
	goAwayProtoErr

	// goAwayInternalErr indicates an internal error.
	goAwayInternalErr
)

const (
	sizeOfVersion  = 1
	sizeOfType     = 1
	sizeOfFlags    = 2
	sizeOfStreamID = 4
	sizeOfLength   = 4
	headerSize     = sizeOfVersion + sizeOfType + sizeOfFlags +
		sizeOfStreamID + sizeOfLength
)

// header represents the 12-byte Yamux frame header backed by a byte slice.
// All fields are encoded in network byte order (big-endian).
type header []byte

// Version returns the protocol version stored in the header.
func (h header) Version() uint8 { return h[0] }

// MsgType returns the message type stored in the header.
func (h header) MsgType() uint8 { return h[1] }

// Flags returns the flags carried by the header.
func (h header) Flags() uint16 { return binary.BigEndian.Uint16(h[2:4]) }

// StreamID returns the stream identifier for the frame.
func (h header) StreamID() uint32 { return binary.BigEndian.Uint32(h[4:8]) }

// Length returns the Length field whose meaning is message-type dependent.
func (h header) Length() uint32 { return binary.BigEndian.Uint32(h[8:12]) }

// String returns a human-readable representation of the header.
func (h header) String() string {
	return fmt.Sprintf("Vsn:%d Type:%d Flags:%d StreamID:%d Length:%d",
		h.Version(), h.MsgType(), h.Flags(), h.StreamID(), h.Length())
}

// encode writes the given values into the header in big-endian order. The
// protocol version is always set to protoVersion.
func (h header) encode(msgType uint8, flags uint16, streamID uint32, length uint32) {
	h[0] = protoVersion
	h[1] = msgType
	binary.BigEndian.PutUint16(h[2:4], flags)
	binary.BigEndian.PutUint32(h[4:8], streamID)
	binary.BigEndian.PutUint32(h[8:12], length)
}
