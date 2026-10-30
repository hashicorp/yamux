// Copyright IBM Corp. 2014, 2025
// SPDX-License-Identifier: MPL-2.0

package yamux

import "time"

// Logger is a abstract of *log.Logger
type Logger interface {
	Print(v ...interface{})
	Printf(format string, v ...interface{})
	Println(v ...interface{})
}

// asyncSendErr is used to try an async send of an error
func asyncSendErr(ch chan error, err error) {
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
	}
}

// asyncNotify is used to signal a waiting goroutine
func asyncNotify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// min computes the minimum of two values
func min(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

// getTimer returns a timer running for d, reusing a stopped one when it can.
// Timers are kept per session so that one never crosses a testing/synctest bubble.
func (s *Session) getTimer(d time.Duration) *time.Timer {
	if t, ok := s.timers.Get().(*time.Timer); ok {
		t.Reset(d)
		return t
	}
	return time.NewTimer(d)
}

// putTimer stops t and keeps it for reuse. The drain is a no-op with Go 1.23
// timers but still needed when a program runs with asynctimerchan=1.
func (s *Session) putTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	s.timers.Put(t)
}
