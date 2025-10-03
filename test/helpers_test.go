package yamux_test

import (
	"bytes"
	"crypto/tls"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	yamux "github.com/hashicorp/yamux"
)

// pipeConn is a simple ReadWriteCloser backed by io.Pipe pairs with
// an optional write blocker to simulate stalled writes.
type pipeConn struct {
	reader       *io.PipeReader
	writer       *io.PipeWriter
	writeBlocker sync.Mutex
}

func (p *pipeConn) Read(b []byte) (int, error) { return p.reader.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) {
	p.writeBlocker.Lock()
	defer p.writeBlocker.Unlock()
	return p.writer.Write(b)
}
func (p *pipeConn) Close() error {
	_ = p.reader.Close()
	return p.writer.Close()
}

// Establish two connected ReadWriteClosers using pipes.
func testConnPipe(_ testing.TB) (io.ReadWriteCloser, io.ReadWriteCloser) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	c1 := &pipeConn{reader: r1, writer: w2}
	c2 := &pipeConn{reader: r2, writer: w1}
	return c1, c2
}

// Establish two connected net.Conn over TCP.
func testConnTCP(t testing.TB) (io.ReadWriteCloser, io.ReadWriteCloser) {
	l, err := net.ListenTCP("tcp", nil)
	if err != nil {
		t.Fatalf("error creating listener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	network := l.Addr().Network()
	addr := l.Addr().String()

	var server net.Conn
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		var err error
		server, err = l.Accept()
		if err != nil {
			errCh <- err
			return
		}
	}()

	client, err := net.DialTimeout(network, addr, 10*time.Second)
	if err != nil {
		t.Fatalf("error dialing listener: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := <-errCh; err != nil {
		t.Fatalf("error accepting: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return client, server
}

// TLS-connected pair using repo testdata certificates.
func testConnTLS(t testing.TB) (io.ReadWriteCloser, io.ReadWriteCloser) {
	cert, err := tls.LoadX509KeyPair("../testdata/cert.pem", "../testdata/key.pem")
	if err != nil {
		t.Fatalf("error loading certificate: %v", err)
	}

	l, err := net.ListenTCP("tcp", nil)
	if err != nil {
		t.Fatalf("error creating listener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	var server net.Conn
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		conn, err := l.Accept()
		if err != nil {
			errCh <- err
			return
		}
		server = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
	}()

	client, err := net.DialTimeout(l.Addr().Network(), l.Addr().String(), 10*time.Second)
	if err != nil {
		t.Fatalf("error dialing tls listener: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	tlsClient := tls.Client(client, &tls.Config{InsecureSkipVerify: true})

	if err := <-errCh; err != nil {
		t.Fatalf("error creating tls server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	return tlsClient, server
}

// Helper to build a baseline config for tests.
func testConf() *yamux.Config {
	conf := yamux.DefaultConfig()
	conf.AcceptBacklog = 64
	conf.KeepAliveInterval = 100 * time.Millisecond
	conf.ConnectionWriteTimeout = 250 * time.Millisecond
	return conf
}

func testConfNoKeepAlive() *yamux.Config {
	conf := testConf()
	conf.EnableKeepAlive = false
	return conf
}

// captureLogs directs logs to a buffer and disables LogOutput.
type logCapture struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf == nil {
		l.buf = &bytes.Buffer{}
	}
	return l.buf.Write(p)
}
func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf == nil {
		return ""
	}
	return l.buf.String()
}

func captureLogs(conf *yamux.Config) *logCapture {
	buf := new(logCapture)
	conf.Logger = log.New(buf, "", 0)
	conf.LogOutput = nil
	return buf
}

// Build a client/server session pair.
func testClientServer(t testing.TB) (*yamux.Session, *yamux.Session) {
	client, server := testConnTLS(t)
	return testClientServerConfig(t, client, server, testConf(), testConf())
}

func testClientServerConfig(
	t testing.TB,
	clientConn, serverConn io.ReadWriteCloser,
	clientConf, serverConf *yamux.Config,
) (clientSession *yamux.Session, serverSession *yamux.Session) {
	var err error
	clientSession, err = yamux.Client(clientConn, clientConf)
	if err != nil {
		t.Fatalf("client init: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	serverSession, err = yamux.Server(serverConn, serverConf)
	if err != nil {
		t.Fatalf("server init: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	return clientSession, serverSession
}
