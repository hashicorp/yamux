package yamux

import (
	"bytes"
	"context"
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

	recvBuf  *bytes.Buffer
	recvLock sync.Mutex

	controlErr chan error
	sendErr    chan error
	sendLock   sync.Mutex

	recvNotifyCh chan struct{}
	sendNotifyCh chan struct{}

	readDeadline   atomic.Value // time.Time
	writeDeadline  atomic.Value // time.Time
	cancelDeadline atomic.Value
}

// newStream is used to construct a new stream within
// a given session for an ID
func newStream(session *Session, id uint32, state streamState) *Stream {
	s := &Stream{
		id:           id,
		session:      session,
		state:        state,
		controlErr:   make(chan error, 1),
		sendErr:      make(chan error, 1),
		recvWindow:   initialStreamWindow,
		sendWindow:   initialStreamWindow,
		recvNotifyCh: make(chan struct{}, 1),
		sendNotifyCh: make(chan struct{}, 1),
	}
	s.readDeadline.Store(time.Time{})
	s.writeDeadline.Store(time.Time{})
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
	var cancel <-chan struct{}
	cancelval := s.cancelDeadline.Load()
	if cancelval != nil {
		cancel = cancelval.(<-chan struct{})
	}
	return s.read(cancel, b)
}

func (s *Stream) read(cancel <-chan struct{}, b []byte) (n int, err error) {
START:
	s.stateLock.Lock()
	switch s.state {
	case streamLocalClose:
		fallthrough
	case streamRemoteClose:
		fallthrough
	case streamClosed:
		s.recvLock.Lock()
		if s.recvBuf == nil || s.recvBuf.Len() == 0 {
			s.recvLock.Unlock()
			s.stateLock.Unlock()
			return 0, io.EOF
		}
		n, _ = s.recvBuf.Read(b)
		s.recvLock.Unlock()

		s.stateLock.Unlock()
		return n, nil
	case streamReset:
		s.stateLock.Unlock()
		return 0, ErrConnectionReset
	}
	s.stateLock.Unlock()

	// If there is no data available, block
	s.recvLock.Lock()
	if s.recvBuf == nil || s.recvBuf.Len() == 0 {
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
	var timer *time.Timer
	readDeadline := s.readDeadline.Load().(time.Time)
	if !readDeadline.IsZero() {
		delay := readDeadline.Sub(time.Now())
		timer = time.NewTimer(delay)
		timeout = timer.C
	}
	select {
	case <-s.recvNotifyCh:
		if timer != nil {
			timer.Stop()
		}
		goto START
	case <-cancel:
		return 0, ErrCallCanceled
	case <-timeout:
		return 0, ErrTimeout
	}
}

// Write is used to write to the stream
func (s *Stream) Write(b []byte) (n int, err error) {
	s.sendLock.Lock()
	defer s.sendLock.Unlock()
	total := 0

	var cancel <-chan struct{}
	cancelval := s.cancelDeadline.Load()
	if cancelval != nil {
		cancel = cancelval.(<-chan struct{})
	}

	for total < len(b) {
		n, err := s.write(cancel, b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// write is used to write to the stream, may return on
// a short write.
func (s *Stream) write(cancel <-chan struct{}, b []byte) (n int, err error) {
	var flags uint16
	var max uint32
	var body io.Reader
	var ready sendReady
	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.session.config.ConnectionWriteTimeout)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerPool.Put(t)
	}()

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
	window := atomic.LoadUint32(&s.sendWindow)
	if window == 0 {
		goto WAIT
	}

	// Determine the flags if any
	flags = s.sendFlags()

	// Send up to our send window
	max = min(window, uint32(len(b)))
	body = bytes.NewReader(b[:max])

	// Send the header
	ready.Body = body
	ready.Err = s.sendErr
	ready.Hdr.encode(typeData, flags, s.id, max)
	if err = s.session.waitForSendErrTimeout(cancel, timer.C, &ready); err != nil {
		return 0, err
	}

	// Reduce our send window
	atomic.AddUint32(&s.sendWindow, ^uint32(max-1))

	// Unlock
	return int(max), err

WAIT:
	var timeout <-chan time.Time
	writeDeadline := s.writeDeadline.Load().(time.Time)
	if !writeDeadline.IsZero() {
		delay := writeDeadline.Sub(time.Now())
		timeout = time.After(delay)
	}
	select {
	case <-s.sendNotifyCh:
		goto START
	case <-cancel:
		return 0, ErrCallCanceled
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
	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.session.config.ConnectionWriteTimeout)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerPool.Put(t)
	}()

	return s.sendWindowUpdateTimeout(nil, timer.C)
}

func (s *Stream) sendWindowUpdateTimeout(cancel <-chan struct{}, timeout <-chan time.Time) error {
	// Determine the delta update
	max := s.session.config.MaxStreamWindowSize
	var bufLen uint32
	s.recvLock.Lock()
	if s.recvBuf != nil {
		bufLen = uint32(s.recvBuf.Len())
	}
	delta := (max - bufLen) - s.recvWindow

	// Determine the flags if any
	flags := s.sendFlags()

	// Check if we can omit the update
	if delta < (max/2) && flags == 0 {
		s.recvLock.Unlock()
		return nil
	}

	// Update our window
	s.recvWindow += delta
	s.recvLock.Unlock()

	// Send the header
	ready := sendReady{Body: nil, Err: s.controlErr}
	ready.Hdr.encode(typeWindowUpdate, flags, s.id, delta)
	if err := s.session.waitForSendErrTimeout(cancel, timeout, &ready); err != nil {
		return err
	}
	return nil
}

// sendClose is used to send a FIN
func (s *Stream) sendClose() error {
	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.session.config.ConnectionWriteTimeout)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerPool.Put(t)
	}()

	flags := s.sendFlags()
	flags |= flagFIN

	ready := sendReady{Body: nil, Err: s.controlErr}
	ready.Hdr.encode(typeWindowUpdate, flags, s.id, 0)
	if err := s.session.waitForSendErrTimeout(nil, timer.C, &ready); err != nil {
		return err
	}
	return nil
}

// Close is used to close the stream
func (s *Stream) Close() error {
	closeStream := false
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
		closeStream = true
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
	if closeStream {
		s.session.closeStream(s.id)
	}
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
	// Close the stream without holding the state lock
	closeStream := false
	defer func() {
		if closeStream {
			s.session.closeStream(s.id)
			s.notifyWaiting()
		}
	}()

	s.stateLock.Lock()
	defer s.stateLock.Unlock()
	if flags&flagACK == flagACK {
		if s.state == streamSYNSent {
			s.state = streamEstablished
		}
		s.session.establishStream(s.id)
	}
	if flags&flagFIN == flagFIN {
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
			closeStream = true
		default:
			s.session.logger.Printf("[ERR] yamux: unexpected FIN flag in state %d", s.state)
			return ErrUnexpectedFlag
		}
	}
	if flags&flagRST == flagRST {
		s.state = streamReset
		closeStream = true
	}
	return nil
}

// notifyWaiting notifies all the waiting channels
func (s *Stream) notifyWaiting() {
	asyncNotify(s.recvNotifyCh)
	asyncNotify(s.sendNotifyCh)
}

// incrSendWindow updates the size of our send window
func (s *Stream) incrSetWindow(hdr header) {
	// Increase window, unblock a sender
	atomic.AddUint32(&s.sendWindow, hdr.Length())
	asyncNotify(s.sendNotifyCh)
}

// readData is used to handle a data frame
func (s *Stream) readData(length uint32, flags uint16, conn io.Reader) error {
	// Check that our recv window is not exceeded
	if length == 0 {
		return nil
	}

	// Copy into buffer
	s.recvLock.Lock()

	if length > s.recvWindow {
		s.session.logger.Printf("[ERR] yamux: receive window exceeded (stream: %d, remain: %d, recv: %d)", s.id, s.recvWindow, length)
		s.recvLock.Unlock()
		return ErrRecvWindowExceeded
	}

	if s.recvBuf == nil {
		// Allocate the receive buffer just-in-time to fit the full data frame.
		// This way we can read in the whole packet without further allocations.
		s.recvBuf = bytes.NewBuffer(make([]byte, 0, length))
	}
	if _, err := io.CopyN(s.recvBuf, conn, int64(length)); err != nil {
		s.session.logger.Printf("[ERR] yamux: Failed to read stream data: %v", err)
		s.recvLock.Unlock()
		return err
	}

	// Decrement the receive window
	s.recvWindow -= length
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
	s.readDeadline.Store(t)
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.Store(t)
	return nil
}

// SetContext set stream context
func (s *Stream) SetContext(ctx context.Context) {
	if ctx != nil {
		s.cancelDeadline.Store(ctx.Done())
	} else {
		s.cancelDeadline.Store(nil)
	}
}

// Shrink is used to compact the amount of buffers utilized
// This is useful when using Yamux in a connection pool to reduce
// the idle memory utilization.
func (s *Stream) Shrink() {
	s.recvLock.Lock()
	if s.recvBuf != nil && s.recvBuf.Len() == 0 {
		s.recvBuf = nil
	}
	s.recvLock.Unlock()
}
