package yamux

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Session wraps a reliable, ordered byte stream (for example a TCP
// connection) and multiplexes it into multiple logical streams. Session is
// safe for concurrent use by multiple goroutines.
type Session struct {
	// remoteGoAway indicates the remote side does not want further
	// connections. Must be first for alignment.
	remoteGoAway int32

	// localGoAway indicates that we should stop
	// accepting further connections. Must be first for alignment.
	localGoAway int32

	// nextStreamID is the next stream we should
	// send. This depends if we are a client/server.
	nextStreamID uint32

	// config holds our configuration
	config *Config

	// logger is used for our logs
	logger Logger

	// conn is the underlying connection
	conn io.ReadWriteCloser

	// bufRead is a buffered reader
	bufRead *bufio.Reader

	// pings is used to track inflight pings
	pings    map[uint32]chan struct{}
	pingID   uint32
	pingLock sync.Mutex

	// streams maps a stream id to a stream, and inflight has an entry
	// for any outgoing stream that has not yet been established. Both are
	// protected by streamLock.
	streams    map[uint32]*Stream
	inflight   map[uint32]struct{}
	streamLock sync.Mutex

	// synCh acts like a semaphore. It is sized to the AcceptBacklog which
	// is assumed to be symmetric between the client and server. This allows
	// the client to avoid exceeding the backlog and instead blocks the open.
	synCh chan struct{}

	// acceptCh is used to pass ready streams to the client
	acceptCh chan *Stream

	// sendCh is used to mark a stream as ready to send,
	// or to send a header out directly.
	sendCh chan *sendReady

	// pingReplyCh queues ping reply IDs to avoid unbounded goroutines.
	pingReplyCh chan uint32

	// recvDoneCh is closed when recv() exits to avoid a race
	// between stream registration and stream shutdown
	recvDoneCh chan struct{}
	sendDoneCh chan struct{}

	// shutdown is used to safely close a session
	shutdown        bool
	shutdownErr     error
	shutdownCh      chan struct{}
	shutdownLock    sync.Mutex
	shutdownErrLock sync.Mutex

	// remoteGoAwayCh is closed when we receive a normal go-away from remote.
	remoteGoAwayCh chan struct{}
}

// sendReady is used to either mark a stream as ready
// or to directly send a header
type sendReady struct {
	Hdr  []byte
	mu   sync.Mutex // Protects Body from unsafe reads.
	Body []byte
	Err  chan error
}

// newSession is used to construct a new session
func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	logger := config.Logger
	if logger == nil {
		logger = log.New(config.LogOutput, "", log.LstdFlags)
	}

	s := &Session{
		config: config,
		logger: logger,
		conn:   conn,
		bufRead: func() *bufio.Reader {
			if config.ReadBufferSize > 0 {
				return bufio.NewReaderSize(conn, config.ReadBufferSize)
			}
			return bufio.NewReader(conn)
		}(),
		pings:          make(map[uint32]chan struct{}),
		streams:        make(map[uint32]*Stream),
		inflight:       make(map[uint32]struct{}),
		synCh:          make(chan struct{}, config.AcceptBacklog),
		acceptCh:       make(chan *Stream, config.AcceptBacklog),
		sendCh:         make(chan *sendReady, 64),
		pingReplyCh:    make(chan uint32, 128),
		recvDoneCh:     make(chan struct{}),
		sendDoneCh:     make(chan struct{}),
		shutdownCh:     make(chan struct{}),
		remoteGoAwayCh: make(chan struct{}),
	}
	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 2
	}
	go s.recv()
	go s.send()
	go s.pingReplyLoop()
	if config.EnableKeepAlive {
		go s.keepalive()
	}
	return s
}

// withWriteDeadline sets a write deadline on the underlying connection if supported
// and returns a function to clear it.
func (s *Session) withWriteDeadline() func() {
	type writeDeadliner interface{ SetWriteDeadline(time.Time) error }
	if s.config.ConnectionWriteTimeout > 0 {
		if c, ok := s.conn.(writeDeadliner); ok {
			_ = c.SetWriteDeadline(time.Now().Add(s.config.ConnectionWriteTimeout))
			return func() { _ = c.SetWriteDeadline(time.Time{}) }
		}
	}
	return func() {}
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	select {
	case <-s.shutdownCh:
		return true
	default:
		return false
	}
}

// CloseChan returns a read-only channel which is closed as
// soon as the session is closed.
func (s *Session) CloseChan() <-chan struct{} {
	return s.shutdownCh
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	s.streamLock.Lock()
	num := len(s.streams)
	s.streamLock.Unlock()
	return num
}

// Open is used to create a new stream as a net.Conn
func (s *Session) Open() (net.Conn, error) {
	conn, err := s.OpenStream()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// OpenStream is used to create a new stream
func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, ErrSessionShutdown
	}
	if atomic.LoadInt32(&s.remoteGoAway) == 1 {
		return nil, ErrRemoteGoAway
	}

	// Block if we have too many inflight SYNs
	select {
	case s.synCh <- struct{}{}:
	case <-s.shutdownCh:
		return nil, ErrSessionShutdown
	}

GET_ID:
	// Get an ID, and check for stream exhaustion
	id := atomic.LoadUint32(&s.nextStreamID)
	if id >= math.MaxUint32-1 {
		return nil, ErrStreamsExhausted
	}
	if !atomic.CompareAndSwapUint32(&s.nextStreamID, id, id+2) {
		goto GET_ID
	}

	// Register the stream
	stream := newStream(s, id, streamInit)
	s.streamLock.Lock()
	s.streams[id] = stream
	s.inflight[id] = struct{}{}
	s.streamLock.Unlock()

	if s.config.StreamOpenTimeout > 0 {
		go s.setOpenTimeout(stream)
	}

	// Send the window update to create
	if err := stream.sendWindowUpdate(); err != nil {
		select {
		case <-s.synCh:
		default:
			s.logger.Printf("[ERR] yamux: aborted stream open without inflight syn semaphore")
		}
		return nil, err
	}
	return stream, nil
}

// setOpenTimeout implements a timeout for streams that are opened but not established.
// If the StreamOpenTimeout is exceeded we assume the peer is unable to ACK,
// and close the session.
// The number of running timers is bounded by the capacity of the synCh.
func (s *Session) setOpenTimeout(stream *Stream) {
	timer := time.NewTimer(s.config.StreamOpenTimeout)
	defer timer.Stop()

	select {
	case <-stream.establishCh:
		return
	case <-s.shutdownCh:
		return
	case <-timer.C:
		// Timeout reached while waiting for ACK.
		// Close the session to force connection re-establishment.
		s.logger.Printf("[ERR] yamux: aborted stream open (destination=%s): %v", s.RemoteAddr().String(), ErrTimeout.err)
		_ = s.Close()
	}
}

// Accept blocks until the next incoming stream is available and returns a
// net.Conn for it. If a remote GoAway has been received and no streams are
// pending, Accept returns ErrRemoteGoAway. If the session closes, the
// session shutdown error is returned.
func (s *Session) Accept() (net.Conn, error) {
	conn, err := s.AcceptStream()
	if err != nil {
		return nil, err
	}
	return conn, err
}

// prepareAcceptedStream sends the initial window update for a newly
// accepted stream and handles cleanup on error.
func (s *Session) prepareAcceptedStream(stream *Stream) (*Stream, error) {
	if err := stream.sendWindowUpdate(); err != nil {
		// Cleanup the stream on initial ACK failure
		stream.forceClose()
		s.closeStream(stream.id)
		return nil, err
	}
	return stream, nil
}

// acceptStreamInternal tries to return a pending stream immediately and,
// if none is pending, blocks until a stream is available or termination
// conditions occur. If a non-nil context is provided, its cancellation is
// respected while blocking.
func (s *Session) acceptStreamInternal(ctx context.Context) (*Stream, error) {
	// Prefer any pending stream immediately.
	select {
	case stream := <-s.acceptCh:
		return s.prepareAcceptedStream(stream)
	default:
	}

	for {
		select {
		case stream := <-s.acceptCh:
			return s.prepareAcceptedStream(stream)
		case <-s.remoteGoAwayCh:
			return nil, ErrRemoteGoAway
		case <-s.shutdownCh:
			return nil, s.shutdownErr
		default:
			if ctx != nil {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case stream := <-s.acceptCh:
					return s.prepareAcceptedStream(stream)
				case <-s.remoteGoAwayCh:
					return nil, ErrRemoteGoAway
				case <-s.shutdownCh:
					return nil, s.shutdownErr
				}
			}
			// If no context is provided, loop back and wait on the main select again.
		}
	}
}

// AcceptStream blocks until the next incoming stream is available and returns
// the concrete *Stream value. Behavior matches Accept.
func (s *Session) AcceptStream() (*Stream, error) {
	return s.acceptStreamInternal(context.Background())
}

// AcceptStreamWithContext blocks until the next incoming stream is available
// or the provided context is done. It prefers pending streams immediately and
// returns ErrRemoteGoAway if a remote GoAway has been received and there are
// no pending streams.
func (s *Session) AcceptStreamWithContext(ctx context.Context) (*Stream, error) {
	return s.acceptStreamInternal(ctx)
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

	s.shutdownErrLock.Lock()
	if s.shutdownErr == nil {
		s.shutdownErr = ErrSessionShutdown
	}
	s.shutdownErrLock.Unlock()

	close(s.shutdownCh)

	_ = s.conn.Close()
	<-s.recvDoneCh

	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	for _, stream := range s.streams {
		stream.forceClose()
	}
	<-s.sendDoneCh
	return nil
}

// exitErr is used to handle an error that is causing the
// session to terminate.
func (s *Session) exitErr(err error) {
	s.shutdownErrLock.Lock()
	if s.shutdownErr == nil {
		s.shutdownErr = err
	}
	s.shutdownErrLock.Unlock()
	_ = s.Close()
}

// GoAway can be used to prevent accepting further
// connections. It does not close the underlying conn.
func (s *Session) GoAway() error {
	return s.waitForSend(s.goAway(goAwayNormal), nil)
}

// goAway is used to send a goAway message
func (s *Session) goAway(reason uint32) header {
	atomic.SwapInt32(&s.localGoAway, 1)
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeGoAway, 0, 0, reason)
	return hdr
}

// Ping is used to measure the RTT response time
func (s *Session) Ping() (time.Duration, error) {
	// Get a channel for the ping
	ch := make(chan struct{})

	// Get a new ping id, mark as pending
	s.pingLock.Lock()
	id := s.pingID
	// Find an ID not currently in use; handle wrap-around naturally.
	for {
		if _, exists := s.pings[id]; !exists {
			break
		}
		id++
	}
	s.pingID = id + 1
	s.pings[id] = ch
	s.pingLock.Unlock()

	// Send the ping request
	hdr := header(make([]byte, headerSize))
	hdr.encode(typePing, flagSYN, 0, id)
	if err := s.waitForSend(hdr, nil); err != nil {
		return 0, err
	}

	// Wait for a response
	start := time.Now()
	timeout := s.config.KeepAliveTimeout
	if timeout <= 0 {
		timeout = s.config.ConnectionWriteTimeout
	}
	select {
	case <-ch:
	case <-time.After(timeout):
		s.pingLock.Lock()
		delete(s.pings, id) // Ignore it if a response comes later.
		s.pingLock.Unlock()
		return 0, ErrTimeout
	case <-s.shutdownCh:
		return 0, ErrSessionShutdown
	}

	// Compute the RTT
	return time.Since(start), nil
}

// keepalive is a long running goroutine that periodically does
// a ping to keep the connection alive.
func (s *Session) keepalive() {
	ticker := time.NewTicker(s.config.KeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_, err := s.Ping()
			if err != nil {
				if err != ErrSessionShutdown {
					s.logger.Printf("[ERR] yamux: keepalive failed: %v", err)
					s.exitErr(ErrKeepAliveTimeout)
				}
				return
			}
		case <-s.shutdownCh:
			return
		}
	}
}

// waitForSendErr waits to send a header, checking for a potential shutdown
func (s *Session) waitForSend(hdr header, body []byte) error {
	errCh := make(chan error, 1)
	return s.waitForSendErr(hdr, body, errCh)
}

// waitForSendErr waits to send a header with optional data, checking for a
// potential shutdown. Since there's the expectation that sends can happen
// in a timely manner, we enforce the connection write timeout here.
func (s *Session) waitForSendErr(hdr header, body []byte, errCh chan error) error {
	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.config.ConnectionWriteTimeout)
	defer func() {
		timer.Stop()
		select {
		case <-timer.C:
		default:
		}
		timerPool.Put(t)
	}()

	ready := &sendReady{Hdr: hdr, Body: body, Err: errCh}
	select {
	case s.sendCh <- ready:
	case <-s.shutdownCh:
		return ErrSessionShutdown
	case <-timer.C:
		return ErrConnectionWriteTimeout
	}

	bodyCopy := func() {
		if body == nil {
			return // A nil body is ignored.
		}

		// In the event of session shutdown or connection write timeout,
		// we need to prevent `send` from reading the body buffer after
		// returning from this function since the caller may re-use the
		// underlying array.
		ready.mu.Lock()
		defer ready.mu.Unlock()

		if ready.Body == nil {
			return // Body was already copied in `send`.
		}
		newBody := make([]byte, len(body))
		copy(newBody, body)
		ready.Body = newBody
	}

	select {
	case err := <-errCh:
		return err
	case <-s.shutdownCh:
		bodyCopy()
		return ErrSessionShutdown
	case <-timer.C:
		bodyCopy()
		return ErrConnectionWriteTimeout
	}
}

// sendNoWait does a send without waiting. Since there's the expectation that
// the send happens right here, we enforce the connection write timeout if we
// can't queue the header to be sent.
func (s *Session) sendNoWait(hdr header) error {
	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.config.ConnectionWriteTimeout)
	defer func() {
		timer.Stop()
		select {
		case <-timer.C:
		default:
		}
		timerPool.Put(t)
	}()

	select {
	case s.sendCh <- &sendReady{Hdr: hdr}:
		return nil
	case <-s.shutdownCh:
		return ErrSessionShutdown
	case <-timer.C:
		return ErrConnectionWriteTimeout
	}
}

// send is a long-running goroutine that serializes header/body writes to the
// underlying connection.
func (s *Session) send() {
	if err := s.sendLoop(); err != nil {
		s.exitErr(err)
	}
}

// sendLoop performs the actual writes. It coalesces header+body via writev when
// available (using net.Buffers) and applies per-write deadlines using
// ConnectionWriteTimeout.
func (s *Session) sendLoop() error {
	defer close(s.sendDoneCh)
	var bodyBuf bytes.Buffer
	for {
		bodyBuf.Reset()

		select {
		case ready := <-s.sendCh:
			// Copy body (if any) into buffer first so we can coalesce header+body.
			ready.mu.Lock()
			if ready.Body != nil {
				// Copy the body into the buffer to avoid holding a lock during the write.
				if _, err := bodyBuf.Write(ready.Body); err != nil {
					ready.Body = nil
					ready.mu.Unlock()
					s.logger.Printf("[ERR] yamux: Failed to copy body into buffer: %v", err)
					asyncSendErr(ready.Err, err)
					return err
				}
				ready.Body = nil
			}
			ready.mu.Unlock()

			// Attempt to coalesce header and body in a single writev when both exist.
			if ready.Hdr != nil && bodyBuf.Len() > 0 {
				cc := s.withWriteDeadline()
				// net.Buffers will use writev on supported platforms via WriteTo.
				buffers := net.Buffers{ready.Hdr, bodyBuf.Bytes()}
				_, err := buffers.WriteTo(s.conn)
				cc()
				if err != nil {
					s.logger.Printf("[ERR] yamux: Failed to write header/body: %v", err)
					asyncSendErr(ready.Err, err)
					return err
				}
			} else {
				// Header-only or body-only paths
				if ready.Hdr != nil {
					cc := s.withWriteDeadline()
					_, err := s.conn.Write(ready.Hdr)
					cc()
					if err != nil {
						s.logger.Printf("[ERR] yamux: Failed to write header: %v", err)
						asyncSendErr(ready.Err, err)
						return err
					}
				}
				if bodyBuf.Len() > 0 {
					cc := s.withWriteDeadline()
					_, err := s.conn.Write(bodyBuf.Bytes())
					cc()
					if err != nil {
						s.logger.Printf("[ERR] yamux: Failed to write body: %v", err)
						asyncSendErr(ready.Err, err)
						return err
					}
				}
			}

			// No error, successful send
			asyncSendErr(ready.Err, nil)
		case <-s.shutdownCh:
			return nil
		}
	}
}

// recv is a long-running goroutine that accepts frames and dispatches them to
// appropriate handlers.
func (s *Session) recv() {
	if err := s.recvLoop(); err != nil {
		s.exitErr(err)
	}
}

// Ensure that the index of the handler (typeData/typeWindowUpdate/etc) matches the message type
var (
	handlers = []func(*Session, header) error{
		typeData:         (*Session).handleStreamMessage,
		typeWindowUpdate: (*Session).handleStreamMessage,
		typePing:         (*Session).handlePing,
		typeGoAway:       (*Session).handleGoAway,
	}
)

// recvLoop reads frame headers, validates version and type, and dispatches
// frames until a fatal error occurs.
func (s *Session) recvLoop() error {
	defer close(s.recvDoneCh)
	hdr := header(make([]byte, headerSize))
	for {
		// Read the header
		if _, err := io.ReadFull(s.bufRead, hdr); err != nil {
			// Avoid noisy logs on common close conditions
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
				s.logger.Printf("[ERR] yamux: Failed to read header: %v", err)
			}
			return err
		}

		// Verify the version
		if hdr.Version() != protoVersion {
			s.logger.Printf("[ERR] yamux: Invalid protocol version: %d", hdr.Version())
			return ErrInvalidVersion
		}

		mt := hdr.MsgType()
		if mt < typeData || mt > typeGoAway {
			return ErrInvalidMsgType
		}

		if err := handlers[mt](s, hdr); err != nil {
			return err
		}
	}
}

// handleStreamMessage handles either a data or window update frame
func (s *Session) handleStreamMessage(hdr header) error {
	// Check for a new stream creation
	id := hdr.StreamID()
	flags := hdr.Flags()
	if flags&flagSYN == flagSYN {
		if err := s.incomingStream(id); err != nil {
			return err
		}
	}

	// Get the stream
	s.streamLock.Lock()
	stream := s.streams[id]
	s.streamLock.Unlock()

	// If we do not have a stream, likely we sent a RST
	if stream == nil {
		// Drain any data on the wire
		if hdr.MsgType() == typeData && hdr.Length() > 0 {
			s.logger.Printf("[WARN] yamux: Discarding data for stream: %d", id)
			if _, err := io.CopyN(io.Discard, s.bufRead, int64(hdr.Length())); err != nil {
				s.logger.Printf("[ERR] yamux: Failed to discard data: %v", err)
				return nil
			}
		} else {
			s.logger.Printf("[WARN] yamux: frame for missing stream: %v", hdr)
		}
		return nil
	}

	// Check if this is a window update
	if hdr.MsgType() == typeWindowUpdate {
		if err := stream.incrSendWindow(hdr, flags); err != nil {
			if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
				s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
			}
			return err
		}
		return nil
	}

	// Read the new data
	if err := stream.readData(hdr, flags, s.bufRead); err != nil {
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return err
	}
	return nil
}

// handlePing handles a typePing frame.
func (s *Session) handlePing(hdr header) error {
	flags := hdr.Flags()
	pingID := hdr.Length()

	// Check if this is a query, respond back in a separate context so we
	// don't interfere with the receiving thread blocking for the write.
	if flags&flagSYN == flagSYN {
		// Enqueue reply without unbounded goroutine creation.
		select {
		case s.pingReplyCh <- pingID:
		default:
			// Drop reply if queue is saturated to avoid resource exhaustion.
			s.logger.Printf("[WARN] yamux: dropping ping reply due to full queue")
		}
		return nil
	}

	// Handle a response
	s.pingLock.Lock()
	ch := s.pings[pingID]
	if ch != nil {
		delete(s.pings, pingID)
		close(ch)
	}
	s.pingLock.Unlock()
	return nil
}

// pingReplyLoop serializes ping replies to avoid spawning a goroutine per ping.
func (s *Session) pingReplyLoop() {
	for {
		select {
		case id := <-s.pingReplyCh:
			hdr := header(make([]byte, headerSize))
			hdr.encode(typePing, flagACK, 0, id)
			if err := s.sendNoWait(hdr); err != nil {
				s.logger.Printf("[WARN] yamux: failed to send ping reply: %v", err)
			}
		case <-s.shutdownCh:
			return
		}
	}
}

// handleGoAway handles a typeGoAway frame.
func (s *Session) handleGoAway(hdr header) error {
	code := hdr.Length()
	switch code {
	case goAwayNormal:
		if atomic.SwapInt32(&s.remoteGoAway, 1) == 0 {
			close(s.remoteGoAwayCh)
		}
	case goAwayProtoErr:
		s.logger.Printf("[ERR] yamux: received protocol error go away")
		return fmt.Errorf("yamux protocol error")
	case goAwayInternalErr:
		s.logger.Printf("[ERR] yamux: received internal error go away")
		return fmt.Errorf("remote yamux internal error")
	default:
		s.logger.Printf("[ERR] yamux: received unexpected go away")
		return fmt.Errorf("unexpected go away received")
	}
	return nil
}

// incomingStream creates a new incoming stream.
func (s *Session) incomingStream(id uint32) error {
	// Reject immediately if we are doing a go away
	if atomic.LoadInt32(&s.localGoAway) == 1 {
		hdr := header(make([]byte, headerSize))
		hdr.encode(typeWindowUpdate, flagRST, id, 0)
		return s.sendNoWait(hdr)
	}

	// Allocate a new stream
	stream := newStream(s, id, streamSYNReceived)

	s.streamLock.Lock()
	defer s.streamLock.Unlock()

	// Check if stream already exists
	if _, ok := s.streams[id]; ok {
		s.logger.Printf("[ERR] yamux: duplicate stream declared")
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return ErrDuplicateStream
	}

	// Register the stream
	s.streams[id] = stream

	// Check if we've exceeded the backlog
	select {
	case s.acceptCh <- stream:
		return nil
	default:
		// Backlog exceeded! RST the stream
		s.logger.Printf("[WARN] yamux: backlog exceeded, forcing connection reset")
		delete(s.streams, id)
		hdr := header(make([]byte, headerSize))
		hdr.encode(typeWindowUpdate, flagRST, id, 0)
		return s.sendNoWait(hdr)
	}
}

// closeStream is used to close a stream once both sides have
// issued a close. If there was an in-flight SYN and the stream
// was not yet established, then this will give the credit back.
func (s *Session) closeStream(id uint32) {
	s.streamLock.Lock()
	if _, ok := s.inflight[id]; ok {
		select {
		case <-s.synCh:
		default:
			s.logger.Printf("[ERR] yamux: SYN tracking out of sync")
		}
		// Remove from inflight tracking to avoid leaks if stream closed pre-ACK
		delete(s.inflight, id)
	}
	delete(s.streams, id)
	s.streamLock.Unlock()
}

// establishStream is used to mark a stream that was in the
// SYN Sent state as established.
func (s *Session) establishStream(id uint32) {
	s.streamLock.Lock()
	if _, ok := s.inflight[id]; ok {
		delete(s.inflight, id)
	} else {
		s.logger.Printf("[ERR] yamux: established stream without inflight SYN (no tracking entry)")
	}
	select {
	case <-s.synCh:
	default:
		s.logger.Printf("[ERR] yamux: established stream without inflight SYN (didn't have semaphore)")
	}
	s.streamLock.Unlock()
}
