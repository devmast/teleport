// Copyright 2023 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resume

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/gravitational/trace"
)

// newWriteSkipConn wraps a [net.Conn] to skip some amount of bytes written to
// it.
func newWriteSkipConn(nc net.Conn, skip uint32) *writeSkipConn {
	c := &writeSkipConn{Conn: nc}
	c.skip.Store(skip)
	return c
}

// writeSkipConn is a [net.Conn] that will skip some amount of bytes written to
// it.
type writeSkipConn struct {
	// skip contains how many bytes we still have to skip while writing.
	skip atomic.Uint32
	// mu protects against concurrent Write calls while skip is nonzero.
	mu sync.Mutex
	net.Conn
}

// Write implements [net.Conn].
func (c *writeSkipConn) Write(b []byte) (int, error) {
	// fast path without locking; as soon as skip is confirmed to be zero, all
	// bets are off about concurrent Write calls
	if c.skip.Load() == 0 {
		return c.Conn.Write(b)
	}

	c.mu.Lock()
	skip := c.skip.Load()
	if skip == 0 {
		// skip became zero while we were blocked on mu, we no longer have to
		// serialize writes
		c.mu.Unlock()
		return c.Conn.Write(b)
	}
	defer c.mu.Unlock()

	if uint64(len(b)) <= uint64(skip) {
		// still check for a write deadline or other errors
		if _, err := c.Conn.Write(nil); err != nil {
			return 0, trace.Wrap(err)
		}

		c.skip.Store(skip - uint32(len(b)))
		return len(b), nil
	}

	n, err := c.Conn.Write(b[skip:])
	if n == 0 && err != nil {
		return 0, trace.Wrap(err)
	}

	c.skip.Store(0)
	return n + int(skip), trace.Wrap(err)
}
