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
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"time"
)

const (
	bufferSize   = 16 * 1024 * 1024
	maxFrameSize = 128 * 1024
	minAdvance   = 16 * 1024
)

func NewConn(localAddr, remoteAddr net.Addr) *Conn {
	return &Conn{
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
	}
}

type Conn struct {
	localAddr  net.Addr
	remoteAddr net.Addr

	mu   sync.Mutex
	cond chan struct{}

	closed bool
	// current is set iff the Conn is attached; it's cleared at the end of run()
	current io.Closer

	readDeadline  time.Time
	writeDeadline time.Time

	// receiveEnd is the absolute position of the end of receiveBuffer.
	receiveEnd uint64
	// receiveBuffer is the data that was received by the peer but not read by
	// the application. Invariants:
	//   len(receiveBuffer) <= bufferSize
	//   receiveEnd - len(receiveBuffer) >= 0
	receiveBuffer []byte

	// replayStart is the absolute position of the beginning of replayBuffer.
	replayStart uint64
	// replayBuffer is the data that was written by the application but not yet
	// acknowledged by the peer. Invariants:
	//   len(replayBuffer) <= bufferSize
	replayBuffer []byte
}

var _ net.Conn = (*Conn)(nil)

func (c *Conn) broadcastLocked() {
	if c.cond == nil {
		return
	}
	close(c.cond)
	c.cond = nil
}

func (c *Conn) waitLocked(timeoutC <-chan time.Time) (timeout bool) {
	if c.cond == nil {
		c.cond = make(chan struct{})
	}
	cond := c.cond
	c.mu.Unlock()
	defer c.mu.Lock()
	select {
	case <-cond:
		return false
	case <-timeoutC:
		return true
	}
}

func (c *Conn) Attach(nc net.Conn) (detached chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.detachLocked()

	detached = make(chan struct{})

	if c.closed {
		nc.Close()
		close(detached)
		return detached
	}

	c.current = nc
	c.broadcastLocked()
	go func() {
		defer close(detached)
		c.run(nc)
	}()

	return detached
}

func (c *Conn) detachLocked() {
	for c.current != nil {
		c.current.Close()
		c.waitLocked(nil)
	}
}

func (c *Conn) Detach() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.detachLocked()
}

func (c *Conn) Status() (closed, attached bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed, c.current != nil
}

func (c *Conn) run(nc net.Conn) {
	readDone := make(chan time.Time)
	defer func() { <-readDone }()

	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.current = nil
		c.broadcastLocked()
	}()
	defer nc.Close()

	c.mu.Lock()
	handshake := binary.AppendUvarint(nil, c.receiveEnd)
	handshake = binary.AppendUvarint(handshake, bufferSize-len64(c.receiveBuffer))
	sentReceiveStart := c.receiveEnd - len64(c.receiveBuffer)
	c.mu.Unlock()

	ncR := bufio.NewReader(nc)

	if _, err := nc.Write(handshake); err != nil {
		return
	}

	remoteReplayStart, err := binary.ReadUvarint(ncR)
	if err != nil {
		return
	}
	remoteWindowSize, err := binary.ReadUvarint(ncR)
	if err != nil {
		return
	}

	c.mu.Lock()
	if remoteReplayStart < c.replayStart || remoteReplayStart > c.replayStart+len64(c.replayBuffer) {
		// we advanced our replay buffer past the read point of the peer, or the
		// read point of the peer is in the future - can't continue, either way
		c.mu.Unlock()
		return
	}
	if c.replayStart != remoteReplayStart {
		c.replayBuffer = c.replayBuffer[remoteReplayStart-c.replayStart:]
		c.replayStart = remoteReplayStart
		c.broadcastLocked()
	}
	c.mu.Unlock()

	// Invariants:
	//   c.replayStart <= remoteReplayStart <= c.replayStart + len(c.replayBuffer)
	//   0 <= c.receiveEnd - len(c.receiveBuffer) <= sentReceiveStart <= c.receiveEnd

	go func() {
		defer close(readDone)

		for {
			advanceWindow, err := binary.ReadUvarint(ncR)
			if err != nil {
				return
			}

			c.mu.Lock()
			// if advanceWindow == 0xffff_ffff_ffff_ffff {
			// 	// explicit close
			// }
			if advanceWindow > 0 {
				if advanceWindow > len64(c.replayBuffer) {
					// trying to move our cursor past the replay buffer?
					c.mu.Unlock()
					return
				}
				remoteWindowSize += advanceWindow
				c.replayBuffer = c.replayBuffer[advanceWindow:]
				c.replayStart += advanceWindow
				c.broadcastLocked()
			}
			c.mu.Unlock()

			frameSize, err := binary.ReadUvarint(ncR)
			if err != nil {
				return
			}

			c.mu.Lock()
			if frameSize == 0 {
				c.mu.Unlock()
				continue
			}
			if frameSize > bufferSize-len64(c.receiveBuffer) || frameSize > maxFrameSize {
				c.mu.Unlock()
				return
			}
			c.receiveBuffer = slices.Grow(c.receiveBuffer, int(frameSize))
			recvBuf := c.receiveBuffer[len(c.receiveBuffer) : len64(c.receiveBuffer)+frameSize]
			c.mu.Unlock()

			n, err := io.ReadFull(ncR, recvBuf)

			if n > 0 {
				c.mu.Lock()
				c.receiveBuffer = append(c.receiveBuffer, recvBuf[:n]...)
				c.receiveEnd += uint64(n)
				c.broadcastLocked()
				c.mu.Unlock()
			}

			if err != nil {
				return
			}
		}
	}()

	for {
		var sendBuf []byte
		var sendAdvance uint64

		c.mu.Lock()
		for {
			sendBuf, sendAdvance = nil, 0
			if remoteWindowSize > 0 && c.replayStart+len64(c.replayBuffer) > remoteReplayStart {
				sendBuf = c.replayBuffer[remoteReplayStart-c.replayStart:]
				sendBuf = sendBuf[:min(remoteWindowSize, maxFrameSize, len64(sendBuf))]
			}
			receiveStart := c.receiveEnd - len64(c.receiveBuffer)
			if receiveStart > sentReceiveStart {
				sendAdvance = receiveStart - sentReceiveStart
			}
			if len(sendBuf) > 0 || sendAdvance > minAdvance {
				break
			}
			if shouldExit := c.waitLocked(readDone); shouldExit {
				c.mu.Unlock()
				return
			}
		}
		c.mu.Unlock()

		metaBuf := binary.AppendUvarint(nil, sendAdvance)
		metaBuf = binary.AppendUvarint(metaBuf, len64(sendBuf))
		if _, err := nc.Write(metaBuf); err != nil {
			return
		}
		if _, err := nc.Write(sendBuf); err != nil {
			return
		}

		c.mu.Lock()
		sentReceiveStart += sendAdvance
		remoteReplayStart += len64(sendBuf)
		remoteWindowSize -= len64(sendBuf)
		c.mu.Unlock()
	}
}

// Close implements [net.Conn].
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.detachLocked()

	if c.closed {
		return nil
	}

	c.closed = true
	c.broadcastLocked()

	return nil
}

// LocalAddr implements [net.Conn].
func (c *Conn) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr implements [net.Conn].
func (c *Conn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func deadlineTimer(deadline time.Time, timer *time.Timer) (*time.Timer, <-chan time.Time) {
	if deadline.IsZero() {
		return timer, nil
	}
	if timer == nil {
		timer = time.NewTimer(time.Until(deadline))
	} else {
		if !timer.Stop() {
			<-timer.C
		}
		timer.Reset(time.Until(deadline))
	}
	return timer, timer.C
}

// SetDeadline implements [net.Conn].
func (c *Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return net.ErrClosed
	}

	c.readDeadline = t
	c.writeDeadline = t
	c.broadcastLocked()
	return nil
}

// SetReadDeadline implements [net.Conn].
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return net.ErrClosed
	}

	c.readDeadline = t
	c.broadcastLocked()
	return nil
}

// SetWriteDeadline implements [net.Conn].
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return net.ErrClosed
	}

	c.writeDeadline = t
	c.broadcastLocked()
	return nil
}

// Read implements [net.Conn].
func (c *Conn) Read(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, net.ErrClosed
	}

	if !c.readDeadline.IsZero() && time.Now().After(c.readDeadline) {
		return 0, os.ErrDeadlineExceeded
	}

	if len(b) == 0 {
		return 0, nil
	}

	var deadlineT *time.Timer
	defer func() {
		if deadlineT != nil {
			deadlineT.Stop()
		}
	}()

	for {
		if len(c.receiveBuffer) > 0 {
			n := copy(b, c.receiveBuffer)
			c.receiveBuffer = c.receiveBuffer[n:]
			c.broadcastLocked()
			return n, nil
		}

		var deadlineC <-chan time.Time
		deadlineT, deadlineC = deadlineTimer(c.readDeadline, deadlineT)

		if timeout := c.waitLocked(deadlineC); timeout {
			return 0, os.ErrDeadlineExceeded
		}

		if c.closed {
			return 0, net.ErrClosed
		}
	}
}

// Write implements [net.Conn].
func (c *Conn) Write(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, net.ErrClosed
	}

	if !c.writeDeadline.IsZero() && time.Now().After(c.writeDeadline) {
		return 0, os.ErrDeadlineExceeded
	}

	if len(b) == 0 {
		return 0, nil
	}

	var deadlineT *time.Timer
	defer func() {
		if deadlineT != nil {
			deadlineT.Stop()
		}
	}()

	for {
		if bufferSize > len(c.replayBuffer) {
			s := min(bufferSize-len(c.replayBuffer), len(b))
			c.replayBuffer = append(c.replayBuffer, b[:s]...)
			b = b[s:]
			n += s
			c.broadcastLocked()
		}

		if len(b) == 0 {
			return n, nil
		}

		var deadlineC <-chan time.Time
		deadlineT, deadlineC = deadlineTimer(c.writeDeadline, deadlineT)

		if timeout := c.waitLocked(deadlineC); timeout {
			return n, os.ErrDeadlineExceeded
		}

		if c.closed {
			return n, net.ErrClosed
		}
	}
}

func len64(s []byte) uint64 {
	return uint64(len(s))
}
