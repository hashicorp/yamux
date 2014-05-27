package yamux

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type streamState int

const (
	streamInit streamState = iota
	streamSYNSent
	streamSYNReceived
	streamEstablished
	streamLocalClose
	streamRemoteClose
	streamClosed
	streamReset
)

// Stream is used to represent a logical stream
// within a session.
type Stream struct {
	recvWindow uint32
	sendWindow uint32

	id      uint32
	session *Session

	state     streamState
	stateLock sync.Mutex

	recvBuf       bytes.Buffer
	waitingBuffer *directBuffer
	recvLock      sync.Mutex

	controlHdr     header
	controlErr     chan error
	controlHdrLock sync.Mutex

	sendHdr  header
	sendErr  chan error
	sendLock sync.Mutex

	recvNotifyCh chan struct{}
	sendNotifyCh chan struct{}

	readDeadline  time.Time
	writeDeadline time.Time
}

type directBuffer struct {
	buf   []byte
	bytes int
}

// newStream is used to construct a new stream within
// a given session for an ID
func newStream(session *Session, id uint32, state streamState) *Stream {
	s := &Stream{
		id:           id,
		session:      session,
		state:        state,
		controlHdr:   header(make([]byte, headerSize)),
		controlErr:   make(chan error, 1),
		sendHdr:      header(make([]byte, headerSize)),
		sendErr:      make(chan error, 1),
		recvWindow:   initialStreamWindow,
		sendWindow:   initialStreamWindow,
		recvNotifyCh: make(chan struct{}, 1),
		sendNotifyCh: make(chan struct{}, 1),
	}
	return s
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
func (s *Stream) Read(b []byte) (n int, err error) {
	defer asyncNotify(s.recvNotifyCh)
	var dBuf *directBuffer

START:
	s.stateLock.Lock()
	switch s.state {
	case streamRemoteClose:
		fallthrough
	case streamClosed:
		if s.recvBuf.Len() == 0 {
			s.stateLock.Unlock()
			return 0, io.EOF
		}
	case streamReset:
		s.stateLock.Unlock()
		return 0, ErrConnectionReset
	}
	s.stateLock.Unlock()

	// If there is no data available, block
	s.recvLock.Lock()
	if s.recvBuf.Len() == 0 {
		// Mark ourself as waiting potentially
		if s.waitingBuffer == nil {
			dBuf = &directBuffer{buf: b}
			s.waitingBuffer = dBuf
		}
		s.recvLock.Unlock()
		goto WAIT
	}

	// Read any bytes
	n, _ = s.recvBuf.Read(b)
	s.recvLock.Unlock()

	// Send a window update potentially
	err = s.sendWindowUpdate()
	return n, err

WAIT:
	var timeout <-chan time.Time
	if !s.readDeadline.IsZero() {
		delay := s.readDeadline.Sub(time.Now())
		timeout = time.After(delay)
	}
	select {
	case <-s.recvNotifyCh:
		if dBuf != nil && dBuf.bytes > 0 {
			err = s.sendWindowUpdate()
			return dBuf.bytes, err
		}
		goto START
	case <-timeout:
		if dBuf != nil && dBuf.bytes > 0 {
			err = s.sendWindowUpdate()
			return dBuf.bytes, err
		}
		return 0, ErrTimeout
	}
}

// Write is used to write to the stream
func (s *Stream) Write(b []byte) (n int, err error) {
	s.sendLock.Lock()
	defer s.sendLock.Unlock()
	total := 0
	for total < len(b) {
		n, err := s.write(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// write is used to write to the stream, may return on
// a short write.
func (s *Stream) write(b []byte) (n int, err error) {
	var flags uint16
	var max uint32
	var body io.Reader
START:
	s.stateLock.Lock()
	switch s.state {
	case streamLocalClose:
		fallthrough
	case streamClosed:
		s.stateLock.Unlock()
		return 0, ErrStreamClosed
	case streamReset:
		s.stateLock.Unlock()
		return 0, ErrConnectionReset
	}
	s.stateLock.Unlock()

	// If there is no data available, block
	if atomic.LoadUint32(&s.sendWindow) == 0 {
		goto WAIT
	}

	// Determine the flags if any
	flags = s.sendFlags()

	// Send up to our send window
	max = min(s.sendWindow, uint32(len(b)))
	body = bytes.NewReader(b[:max])

	// Send the header
	s.sendHdr.encode(typeData, flags, s.id, max)
	if err := s.session.waitForSendErr(s.sendHdr, body, s.sendErr); err != nil {
		return 0, err
	}

	// Reduce our send window
	atomic.AddUint32(&s.sendWindow, ^uint32(max-1))

	// Unlock
	return int(max), err

WAIT:
	var timeout <-chan time.Time
	if !s.writeDeadline.IsZero() {
		delay := s.writeDeadline.Sub(time.Now())
		timeout = time.After(delay)
	}
	select {
	case <-s.sendNotifyCh:
		goto START
	case <-timeout:
		return 0, ErrTimeout
	}
	return 0, nil
}

// sendFlags determines any flags that are appropriate
// based on the current stream state
func (s *Stream) sendFlags() uint16 {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()
	var flags uint16
	switch s.state {
	case streamInit:
		flags |= flagSYN
		s.state = streamSYNSent
	case streamSYNReceived:
		flags |= flagACK
		s.state = streamEstablished
	}
	return flags
}

// sendWindowUpdate potentially sends a window update enabling
// further writes to take place. Must be invoked with the lock.
func (s *Stream) sendWindowUpdate() error {
	s.controlHdrLock.Lock()
	defer s.controlHdrLock.Unlock()

	// Determine the delta update
	max := s.session.config.MaxStreamWindowSize
	delta := max - s.recvWindow

	// Determine the flags if any
	flags := s.sendFlags()

	// Check if we can omit the update
	if delta < (max/2) && flags == 0 {
		return nil
	}

	// Send the header
	s.controlHdr.encode(typeWindowUpdate, flags, s.id, delta)
	if err := s.session.waitForSendErr(s.controlHdr, nil, s.controlErr); err != nil {
		return err
	}

	// Update our window
	s.recvWindow += delta
	return nil
}

// sendClose is used to send a FIN
func (s *Stream) sendClose() error {
	s.controlHdrLock.Lock()
	defer s.controlHdrLock.Unlock()

	flags := s.sendFlags()
	flags |= flagFIN
	s.controlHdr.encode(typeWindowUpdate, flags, s.id, 0)
	if err := s.session.sendNoWait(s.controlHdr); err != nil {
		return err
	}
	return nil
}

// Close is used to close the stream
func (s *Stream) Close() error {
	s.stateLock.Lock()
	switch s.state {
	// Opened means we need to signal a close
	case streamSYNSent:
		fallthrough
	case streamSYNReceived:
		fallthrough
	case streamEstablished:
		s.state = streamLocalClose
		goto SEND_CLOSE

	case streamLocalClose:
	case streamRemoteClose:
		s.state = streamClosed
		s.session.closeStream(s.id, false)
		goto SEND_CLOSE

	case streamClosed:
	case streamReset:
	default:
		panic("unhandled state")
	}
	s.stateLock.Unlock()
	return nil
SEND_CLOSE:
	s.stateLock.Unlock()
	s.sendClose()
	s.notifyWaiting()
	return nil
}

// forceClose is used for when the session is exiting
func (s *Stream) forceClose() {
	s.stateLock.Lock()
	s.state = streamClosed
	s.stateLock.Unlock()
	s.notifyWaiting()
}

// processFlags is used to update the state of the stream
// based on set flags, if any. Lock must be held
func (s *Stream) processFlags(flags uint16) error {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()
	if flags&flagACK == flagACK {
		if s.state == streamSYNSent {
			s.state = streamEstablished
		}

	} else if flags&flagFIN == flagFIN {
		switch s.state {
		case streamSYNSent:
			fallthrough
		case streamSYNReceived:
			fallthrough
		case streamEstablished:
			s.state = streamRemoteClose
			s.notifyWaiting()
		case streamLocalClose:
			s.state = streamClosed
			s.session.closeStream(s.id, true)
			s.notifyWaiting()
		default:
			return ErrUnexpectedFlag
		}
	} else if flags&flagRST == flagRST {
		s.state = streamReset
		s.session.closeStream(s.id, true)
		s.notifyWaiting()
	}
	return nil
}

// notifyWaiting notifies all the waiting channels
func (s *Stream) notifyWaiting() {
	asyncNotify(s.recvNotifyCh)
	asyncNotify(s.sendNotifyCh)
}

// incrSendWindow updates the size of our send window
func (s *Stream) incrSendWindow(hdr header, flags uint16) error {
	if err := s.processFlags(flags); err != nil {
		return err
	}

	// Increase window, unblock a sender
	atomic.AddUint32(&s.sendWindow, hdr.Length())
	asyncNotify(s.sendNotifyCh)
	return nil
}

// readData is used to handle a data frame
func (s *Stream) readData(hdr header, flags uint16, conn io.Reader) error {
	if err := s.processFlags(flags); err != nil {
		return err
	}

	// Check that our recv window is not exceeded
	length := hdr.Length()
	if length == 0 {
		return nil
	}
	if length > atomic.LoadUint32(&s.recvWindow) {
		return ErrRecvWindowExceeded
	}

	// Decrement the receive window
	atomic.AddUint32(&s.recvWindow, ^uint32(length-1))

	// Wrap in a limited reader
	conn = &io.LimitedReader{R: conn, N: int64(length)}

	// Copy into waiting buffer if any
	s.recvLock.Lock()
	if s.waitingBuffer != nil {
		n, err := conn.Read(s.waitingBuffer.buf)
		s.waitingBuffer.bytes = n
		s.waitingBuffer = nil
		if err != nil {
			s.recvLock.Unlock()
			return err
		}
	}

	// Copy to our buffer anything left
	if _, err := io.Copy(&s.recvBuf, conn); err != nil {
		s.recvLock.Unlock()
		return err
	}
	s.recvLock.Unlock()

	// Unblock any readers
	asyncNotify(s.recvNotifyCh)
	return nil
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
