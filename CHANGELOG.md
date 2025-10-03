## Unreleased

### Added

- `Stream.CloseWrite`: exposes half-close semantics to callers that probe for a `CloseWrite()` method (e.g., SOCKS5). Internally delegates to `Close()`. (Resolves #154)
- Coalesced header+body writes: the session send loop now attempts to write header and body together via `writev` when available (using `net.Buffers`), reducing syscalls under load.
- Configurable reader size: new `Config.ReadBufferSize` allows tuning the size of the buffered reader wrapping the underlying connection.
- Dedicated keepalive timeout: new `Config.KeepAliveTimeout` bounds how long we wait for a Ping ACK during keep-alive (falls back to `ConnectionWriteTimeout` if zero).
  - DefaultConfig sets `KeepAliveTimeout` to 10s.

### Changes

- Config validation: `VerifyConfig` only requires a positive `KeepAliveInterval` when `EnableKeepAlive` is true. Aligns with intent of PR #153.
- Config validation tightened: `ConnectionWriteTimeout` must be positive; `KeepAliveTimeout` must be >= 0 when keepalive are enabled.
- Keep-alive now use a `time.Ticker` and call `Stop()` on shutdown to reduce allocations and dangling timers.
- Connection write "safety valve": the `ConnectionWriteTimeout` is applied as a per-write deadline for both frame headers and bodies in the session send loop.
- Ping replies are serialized through a bounded queue to prevent unbounded goroutine creation under ping floods.
- Accept semantics on remote GoAway: `AcceptStream` and `AcceptStreamWithContext` now unblock and return `ErrRemoteGoAway` after a remote GoAway is received when no pending streams are queued. (Closes #17)
 - Opportunistic shrink: streams shrink their receive buffers on close or remote half-close when empty to reduce idle memory.

### Fixed

- Keepalive timeout hang: sessions now close promptly on keepalive ping failure and per-write deadlines prevent indefinite blocking in the send loop. (Addresses #149)
- Stream close timers: `forceClose()` now stops and clears the internal close timer to avoid late callbacks and potential leaks on session shutdown.
- Accept cleanup: if the initial ACK/window-update send fails during `AcceptStream`, the newly created stream is force-closed and removed to avoid lingering state.
- Inflight tracking cleanup: `closeStream` now deletes pre-ACK inflight entries to prevent tracking leaks.
- Less noisy shutdown logs: `recvLoop` uses `errors.Is` to filter common close conditions instead of string matching.
- Robust close behavior: `Stream.Close()` no longer panics on an unexpected state and instead returns an error.

### Docs

- Documented that `ErrTimeout` can be interpreted as a backpressure signal for callers to implement retries/throttling.
- README gained a Tuning section covering buffer sizes, timeouts, windows, backlog and operational notes.

### Tests

- All tests moved under `test/` as black-box tests.
- New coverage includes: keepalive config validation, basic ping, write deadline application, accept cleanup on ACK failure, remote GoAway behavior, `CloseWrite` half-close, a small-stream throughput benchmark, a concurrent stress test opening many streams, and a multi-transport (Pipe/TCP/TLS) end-to-end test.
- CI updated to compute coverage across packages using `-coverpkg=./...` so coverage reflects the library even with tests in an external `test` package.

### Security

- Improved resilience to resource exhaustion via a bounded ping-reply queue and stricter per-write timeouts.
