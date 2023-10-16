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

	"github.com/sirupsen/logrus"
)

const (
	readBufferSize  = 128 * 1024
	writeBufferSize = 2 * 1024 * 1024

	maxFrameSize = 128 * 1024
)

func NewConn(localAddr, remoteAddr net.Addr) *Conn {
	return &Conn{
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
	}
}

type Conn struct {
	mu   sync.Mutex
	cond chan struct{}

	localAddr    net.Addr
	remoteAddr   net.Addr
	allowRoaming bool

	closed bool
	// current is set iff the Conn is attached; it's cleared at the end of run()
	current io.Closer

	readDeadline  time.Time
	writeDeadline time.Time

	// receiveEnd is the absolute position of the end of receiveBuffer.
	receiveEnd uint64
	// receiveBuffer is the data that was received by the peer but not read by
	// the application. Its size should not exceed readBufferSize
	receiveBuffer []byte

	// replayStart is the absolute position of the beginning of replayBuffer.
	replayStart uint64
	// replayBuffer is the data that was written by the application but not yet
	// acknowledged by the peer. Its size should not exceed writeBufferSize.
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

func (c *Conn) AllowRoaming() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allowRoaming = true
}

// sameTCPSourceAddress returns true if the two [net.Addr]s are both
// [*net.TCPAddr] (like golang.org/x/crypto/ssh.checkSourceAddress requires) and
// if the IP addresses are equal.
func sameTCPSourceAddress(addr1, addr2 net.Addr) bool {
	t1, ok := addr1.(*net.TCPAddr)
	if !ok {
		return false
	}
	t2, ok := addr2.(*net.TCPAddr)
	if !ok {
		return false
	}

	return t1.IP.Equal(t2.IP)
}

func (c *Conn) Attach(nc net.Conn) (detached chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	detached = make(chan struct{})

	localAddr := nc.LocalAddr()
	remoteAddr := nc.RemoteAddr()

	if !c.allowRoaming && !sameTCPSourceAddress(c.remoteAddr, remoteAddr) {
		logrus.Error("not same TCP source address")
		nc.Close()
		close(detached)
		return detached
	}

	c.detachLocked()

	if c.closed {
		logrus.Error("actually closed")
		nc.Close()
		close(detached)
		return detached
	}

	c.localAddr = localAddr
	c.remoteAddr = remoteAddr

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
	var readDone chan time.Time
	defer func() {
		if readDone != nil {
			<-readDone
		}
	}()

	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.current = nil
		c.broadcastLocked()
	}()
	defer nc.Close()

	c.mu.Lock()
	sentReceivePosition := c.receiveEnd
	c.mu.Unlock()

	ncR := bufio.NewReader(nc)

	if _, err := nc.Write(binary.AppendUvarint(nil, sentReceivePosition)); err != nil {
		logrus.Error("failed handshake write")
		return
	}

	peerReceivePosition, err := binary.ReadUvarint(ncR)
	if err != nil {
		logrus.Error("failed readuvarint replaystart")
		return
	}

	c.mu.Lock()
	if peerReceivePosition < c.replayStart || peerReceivePosition > c.replayStart+len64(c.replayBuffer) {
		// we advanced our replay buffer past the read point of the peer, or the
		// read point of the peer is in the future - can't continue, either way
		c.mu.Unlock()
		logrus.Error("incompatible resume")
		return
	}
	if c.replayStart != peerReceivePosition {
		c.replayBuffer = c.replayBuffer[peerReceivePosition-c.replayStart:]
		c.replayStart = peerReceivePosition
		c.broadcastLocked()
	}
	c.mu.Unlock()

	logrus.Error("handshake completed successfully")

	readDone = make(chan time.Time)
	writeDone := make(chan time.Time)
	go func() {
		defer close(readDone)

		for {
			ack, err := binary.ReadUvarint(ncR)
			if err != nil {
				logrus.Error("failed readuvarint ack")
				return
			}

			if ack > 0 {
				// if ack == 0xffff_ffff_ffff_ffff {
				// 	// explicit close
				// }
				c.mu.Lock()

				if ack > len64(c.replayBuffer) {
					// trying to move our cursor past the replay buffer?
					c.mu.Unlock()
					logrus.Error("ack past end of replay buffer")
					return
				}
				c.replayBuffer = c.replayBuffer[ack:]
				c.replayStart += ack
				c.broadcastLocked()

				c.mu.Unlock()
			}

			frameSize, err := binary.ReadUvarint(ncR)
			if err != nil {
				logrus.Error("failed readuvarint framesize")
				return
			}

			if frameSize > maxFrameSize {
				logrus.Error("oversized frame")
				return
			}

			c.mu.Lock()
			for frameSize > 0 {
				for len(c.receiveBuffer) >= readBufferSize {
					if shouldExit := c.waitLocked(writeDone); shouldExit {
						c.mu.Unlock()
						logrus.Error("writeDone")
						return
					}
				}
				receiveTailSize := min(readBufferSize-len(c.receiveBuffer), int(frameSize))
				c.receiveBuffer = slices.Grow(c.receiveBuffer, receiveTailSize)
				receiveTail := c.receiveBuffer[len(c.receiveBuffer) : len(c.receiveBuffer)+receiveTailSize]
				c.mu.Unlock()

				n, err := io.ReadFull(ncR, receiveTail)

				c.mu.Lock()
				if n > 0 {
					c.receiveBuffer = append(c.receiveBuffer, receiveTail[:n]...)
					c.receiveEnd += uint64(n)
					frameSize -= uint64(n)
					c.broadcastLocked()
				}

				if err != nil {
					c.mu.Unlock()
					logrus.Error("failed read frame")
					return
				}
			}
			c.mu.Unlock()
		}
	}()

	defer close(writeDone)
	for {
		var sendAck uint64
		var sendBuf []byte

		c.mu.Lock()
		for {
			sendBuf = nil
			if c.replayStart+len64(c.replayBuffer) > peerReceivePosition {
				sendBuf = c.replayBuffer[peerReceivePosition-c.replayStart:]
				sendBuf = sendBuf[:min(len(sendBuf), maxFrameSize)]
			}
			sendAck = c.receiveEnd - sentReceivePosition
			if len(sendBuf) > 0 || sendAck > 0 {
				break
			}
			if shouldExit := c.waitLocked(readDone); shouldExit {
				c.mu.Unlock()
				logrus.Error("shouldExit")
				return
			}
		}
		c.mu.Unlock()

		metaBuf := binary.AppendUvarint(nil, sendAck)
		metaBuf = binary.AppendUvarint(metaBuf, len64(sendBuf))
		if _, err := nc.Write(metaBuf); err != nil {
			logrus.Error("failed write meta")
			return
		}
		if _, err := nc.Write(sendBuf); err != nil {
			logrus.Error("failed write buf")
			return
		}

		c.mu.Lock()
		sentReceivePosition += sendAck
		peerReceivePosition += len64(sendBuf)
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
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localAddr
}

// RemoteAddr implements [net.Conn].
func (c *Conn) RemoteAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
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
		if writeBufferSize > len(c.replayBuffer) {
			s := min(writeBufferSize-len(c.replayBuffer), len(b))
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
