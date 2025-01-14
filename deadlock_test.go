// deadlock_test.go
//
// This test sets up two yamux streams in parallel (A and B). We artificially
// stall the underlying pipeConn so that B's writes fill yamux's channel
// and eventually time out, but we do NOT close the session. This leaves the
// remote side's B.Read call blocked forever, illustrating the deadlock bug.
//
// Meanwhile, stream A is paused and resumed just to illustrate concurrency,
// and that the other streams are unaffected- but it's not strictly
// necessary. The main point is that B times out on write and does *not*
// close the session, so the remote Read is stuck forever.

package yamux

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// We'll use smaller timeouts so the scenario triggers quickly.
const (
	testConnWriteTimeout = 300 * time.Millisecond
	testKeepAlive        = 30 * time.Second // KeepAliveInterval > ConnectionWriteTimeout
)

type msg struct {
	label string
	val   int
}

// monotonicWriter sends a never-ending sequence of json objects with incrementing
// integers, to the provided stream. It optionally pauses when signaled on the pausedCh.
func monotonicWriter(t *testing.T, stream io.WriteCloser, label string, pausedCh <-chan struct{}) error {
	var counter int
	enc := json.NewEncoder(stream)
	for {
		// Optionally pause if channel is signaled
		select {
		case <-pausedCh:
			// wait until resumed
			t.Logf("[%s] writer paused", label)
			<-pausedCh
			t.Logf("[%s] writer resumed", label)
		default:
		}

		err := enc.Encode(msg{label, counter})
		if err != nil {
			return fmt.Errorf("[%s] write error: %w", label, err)
		}
		counter++
	}
}

// monotonicReader reads json messages from the stream, expecting a sequence
// It will error if it sees a gap or anything unexpected.
func monotonicReader(t *testing.T, stream io.Reader, label string) error {
	expected := 0
	dec := json.NewDecoder(stream)
	for {
		var m msg
		// Read one line
		err := dec.Decode(&m)
		if err == io.EOF {
			return io.EOF
		}
		if err != nil {
			return fmt.Errorf("[%s] read error after seeing %d values: %w", label, expected, err)
		}
		if m.label == "" {
			continue
		}
		if m.label != label {
			return fmt.Errorf("[%s] read label mismatch: got %q, want %q", label, m.label, label)
		}
		if m.val != expected {
			return fmt.Errorf("[%s] read value mismatch: got %d, want %d", label, m.val, expected)
		}
		expected++
	}
}

func makeConnPair() (*pipeConn, *pipeConn) {
	read1, write1 := io.Pipe()
	read2, write2 := io.Pipe()
	conn1 := &pipeConn{reader: read1, writer: write2}
	conn2 := &pipeConn{reader: read2, writer: write1}
	return conn1, conn2
}

func failNow(errCh chan error, message string, err error) {
	errCh <- fmt.Errorf("%s: %w", message, err)
}

// TestTimeoutParallel verifies that if one yamux stream times out on write,
// it automatically closes the entire session. This prevents leaving the
// remote side in a deadlocked read if the bug is present.
//
// Steps:
//  1. Create yamux client/server over a pair of pipeConn with small
//     ConnectionWriteTimeout and KeepAliveInterval so we can trigger the
//     scenario quickly.
//  2. We start two parallel streams: stream A and stream B, each in separate
//     goroutines. Each writes an infinite sequence of incrementing JSON
//     objects to the other side, which also reads them in an infinite loop
//     checking correctness.
//  3. We artificially pause A's writer. Then, we BLOCK the pipeConn so that
//     B's writes will fill up the Session's channel and time out on the
//     server side with ErrConnectionWriteTimeout. We do *not* close the
//     session or stream after that, as that reflects common usage of this
//     library.
//  4. We unblock the pipe. Meanwhile, B's reader on the client side is left
//     waiting for data that never arrives (under the bug). The read never
//     completes.
//
// If the bug is fixed (i.e., the code forcibly closes the entire session when
// Stream.Write times out), then the client read on B would quickly see an EOF/reset
// instead of hanging forever.
func TestTimeoutParallel(t *testing.T) {
	// used for waiting at the end of the test
	var wg sync.WaitGroup
	// used for failing now
	errCh := make(chan error, 1)

	// 1. Setup net.Pipe
	serverSide, clientSide := makeConnPair()

	// 2. Make yamux config with small timeouts so we see a quick repro
	serverConf := DefaultConfig()
	serverConf.ConnectionWriteTimeout = testConnWriteTimeout
	serverConf.KeepAliveInterval = testKeepAlive

	clientConf := DefaultConfig()
	clientConf.ConnectionWriteTimeout = testConnWriteTimeout
	clientConf.KeepAliveInterval = testKeepAlive

	// 3. Start server session
	serverSes, err := Server(serverSide, serverConf)
	if err != nil {
		t.Fatalf("yamux server: %v", err)
	}
	defer serverSes.Close()

	// 4. Start client session
	clientSes, err := Client(clientSide, clientConf)
	if err != nil {
		t.Fatalf("yamux client: %v", err)
	}
	defer clientSes.Close()

	// 5. Create two streams from server->client: A and B
	// 6. On the client side, Accept them
	// 7. We'll run two sets of goroutines: each side has a writer and a reader.

	// A signals for "pause/resume"
	pauseA := make(chan struct{}, 1)

	// Start A writer (server side)
	go func() {
		wg.Add(1)
		defer wg.Done()
		streamAserver, err := serverSes.Open()
		if err != nil {
			t.Fatalf("server open stream A: %v", err)
		}
		err = monotonicWriter(t, streamAserver, "A", pauseA)
		if err != nil && !errors.Is(err, ErrStreamClosed) {
			failNow(errCh, "[A-writer-server] received unexpected error", err)
		}
	}()

	// Start A reader (client side)
	go func() {
		wg.Add(1)
		defer wg.Done()
		streamAclient, _ := clientSes.Accept()
		defer streamAclient.Close()
		err = monotonicReader(t, streamAclient, "A")
		if err != nil && !errors.Is(err, io.EOF) {
			failNow(errCh, "[A-reader-client] received unexpected error", err)
		}
	}()

	// Start B writer (server side)
	go func() {
		wg.Add(1)
		defer wg.Done()
		streamBserver, _ := serverSes.Open()
		err = monotonicWriter(t, streamBserver, "B", nil)
		if err != nil && !errors.Is(err, ErrConnectionWriteTimeout) {
			failNow(errCh, "[B-writer-server] received unexpected error", err)
		}
	}()

	// Start B reader (client side)
	go func() {
		wg.Add(1)
		defer wg.Done()
		streamBclient, _ := clientSes.Accept()
		defer streamBclient.Close()
		err = monotonicReader(t, streamBclient, "B")
		if err != nil && !errors.Is(err, io.EOF) {
			failNow(errCh, "[B-reader-client] received unexpected error", err)
		}
	}()

	// 8. Let them run for a moment, then pause A's writer so B's throughput is the only one.
	time.Sleep(100 * time.Millisecond)
	pauseA <- struct{}{} // signal A writer to pause
	t.Log("[test] Paused A writer")
	time.Sleep(100 * time.Millisecond)

	// 9. Stall the pipeConn for a while. This will cause B’s writes to fill the sendCh.
	//    We'll hold the pipe locked for slightly longer than ConnectionWriteTimeout,
	//    but less than ConnectionWriteTimeout + KeepAliveInterval so that the session
	//    doesn't get closed by keepalive. B's Stream.Write should time out, returning
	//    ErrConnectionWriteTimeout.

	blockDuration := testConnWriteTimeout + 100*time.Millisecond // just over the write timeout
	t.Logf("[test] Blocking net.Pipe for ~%v so B hits write timeout...", blockDuration)
	serverSide.writeBlocker.Lock()
	time.Sleep(blockDuration)

	// 10. Unblock the pipeConn
	t.Logf("[test] Unblocking net.Pipe after %v", blockDuration)
	serverSide.writeBlocker.Unlock()
	t.Log("[test] Unblocked net.Pipe")

	// 11. Resume A writer
	pauseA <- struct{}{}
	t.Log("[test] Resumed A writer")

	// 12. Now we wait. Under the bug scenario, B's writer got `ErrConnectionWriteTimeout` but did NOT close
	//     the entire session. B's reader on the client side is stuck waiting forever, so the test will hang.
	//     You can let this run. If you want to see it fail faster, you can run:
	//       go test -timeout=15s .
	//     ...which will kill the test once it sees no progress after 15s.
	//
	// If the code is *fixed* so that the session is forced closed on write-timeout,
	// B's read sees an EOF/reset, and all goroutines exit quickly.

	t.Log("[test] Test is done: either we will hang, or the session is forcibly closed if the bug is fixed.")
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Second)
		wg.Add(1)
		t.Log("[test] still running; if there's no fix, we might be stuck forever. " +
			"Consider letting -timeout forcibly fail the test.")
	}()
	go func() {
		wg.Wait()
		close(errCh)
	}()

	select {
	case errNow, ok := <-errCh:
		if ok {
			t.Fatalf("test failed: %v", errNow)
		}
	}
}
