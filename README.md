# Yamux

Yamux (Yet another Multiplexer) is a multiplexing library for Golang.
It relies on an underlying connection to provide reliability
and ordering, such as TCP or Unix domain sockets, and provides
stream-oriented multiplexing. It is inspired by SPDY but is not
interoperable with it.

Yamux features include:

* Bi-directional streams
  * Streams can be opened by either client or server
  * Useful for NAT traversal
  * Server-side push support
* Flow control
  * Avoid starvation
  * Back-pressure to prevent overwhelming a receiver
* Keep Alive
  * Enables persistent connections over a load balancer
* Efficient
  * Enables thousands of logical streams with low overhead

## Documentation

For complete documentation, see the associated [Godoc](http://godoc.org/github.com/hashicorp/yamux).

## Specification

The full specification for Yamux is provided in the `spec.md` file.
It can be used as a guide to implementors of interoperable libraries.

## Usage

Using Yamux is remarkably simple:

```go

func client() {
    // Get a TCP connection
    conn, err := net.Dial(...)
    if err != nil {
        panic(err)
    }

    // Setup client side of yamux
    session, err := yamux.Client(conn, nil)
    if err != nil {
        panic(err)
    }

    // Open a new stream
    stream, err := session.Open()
    if err != nil {
        panic(err)
    }

    // Stream implements net.Conn
    stream.Write([]byte("ping"))
}

func server() {
    // Accept a TCP connection
    conn, err := listener.Accept()
    if err != nil {
        panic(err)
    }

    // Setup server side of yamux
    session, err := yamux.Server(conn, nil)
    if err != nil {
        panic(err)
    }

    // Accept a stream
    stream, err := session.Accept()
    if err != nil {
        panic(err)
    }

    // Listen for a message
    buf := make([]byte, 4)
    stream.Read(buf)
}

```

## Tuning

Yamux is designed for high-throughput, low-overhead multiplexing. Depending on your workload and network characteristics, consider tuning these parameters via `yamux.Config`:

- ReadBufferSize
  - Controls the size of the buffered reader wrapping the underlying connection.
  - If unset or <= 0, the default `bufio.Reader` size is used.
  - Increase for high-BDP (bandwidth-delay product) links to reduce read syscalls. Typical values: 64 KiB – 512 KiB.

- KeepAliveInterval and KeepAliveTimeout
  - Interval determines how often a keep-alive Ping is sent (default 30s).
  - Timeout bounds how long we wait for the Ping ACK. If zero, falls back to `ConnectionWriteTimeout`.
  - On failure, the session closes with `ErrKeepAliveTimeout`.

- ConnectionWriteTimeout
  - Per-write “safety valve” deadline for header and body writes.
  - Prevents indefinite blocking in the send path and acts as a backpressure timer when the underlying transport stalls.

- MaxStreamWindowSize
  - Upper bound for per-stream receive window. Larger windows can help on high-BDP paths but consume more memory per active stream.

- AcceptBacklog
  - Caps concurrent in-flight stream opens (SYNs). Set symmetrically on both sides. Exceeding the backlog can lead to RSTs of new inbound streams.

- StreamOpenTimeout
  - Max time a newly opened stream will wait for the peer’s ACK before the session is closed to force reconnection. Set to zero to disable.

- StreamCloseTimeout
  - Grace period after a local half-close (FIN) before forcibly resetting the stream to avoid leaks if the peer never responds.

Operational notes
- ErrTimeout may be returned on read/write when deadlines are reached; it often signals backpressure. Callers can implement retries or adaptive throttling.
- Writes coalesce header and body using writev (where available) to reduce syscalls under load.
- For connection pools, consider periodically calling `Shrink()` on idle streams to reclaim buffered memory.


