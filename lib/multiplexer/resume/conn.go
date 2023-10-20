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
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
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
		receive:    window{data: make([]byte, 4096)},
		replay:     window{data: make([]byte, 4096)},
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

	readTimeout  *bool
	writeTimeout *bool
	readTimer    *time.Timer
	writeTimer   *time.Timer

	receive window
	replay  window
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

func (c *Conn) waitLockedContext(ctx context.Context) error {
	if c.cond == nil {
		c.cond = make(chan struct{})
	}
	cond := c.cond
	c.mu.Unlock()
	defer c.mu.Lock()
	select {
	case <-cond:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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

func (c *Conn) Attach(nc net.Conn, new bool) (detached chan error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	detached = make(chan error, 1)

	localAddr := nc.LocalAddr()
	remoteAddr := nc.RemoteAddr()

	if !c.allowRoaming && !sameTCPSourceAddress(c.remoteAddr, remoteAddr) {
		nc.Close()
		detached <- trace.AccessDenied("invalid TCP source address for resumable non-roaming connection")
		return detached
	}

	c.detachLocked()

	if c.closed {
		logrus.Error("actually closed")
		nc.Close()
		detached <- trace.ConnectionProblem(net.ErrClosed, "attaching to a closed resumable connection: %v", net.ErrClosed.Error())
		return detached
	}

	c.localAddr = localAddr
	c.remoteAddr = remoteAddr

	c.current = nc
	c.broadcastLocked()
	go func() {
		detached <- c.run(nc, new)
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

func (c *Conn) run(nc net.Conn, firstRun bool) error {
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.current = nil
		c.broadcastLocked()
	}()
	defer nc.Close()

	// this will succeed if the conn is a [*multiplexer.Conn], for example
	ncReader, ok := nc.(interface {
		io.Reader
		io.ByteReader
	})
	if !ok {
		ncReader = bufio.NewReader(nc)
	}

	c.mu.Lock()
	sentReceivePosition := c.receive.End()
	c.mu.Unlock()

	var peerReceivePosition uint64

	if !firstRun {
		if _, err := nc.Write(binary.AppendUvarint(nil, sentReceivePosition)); err != nil {
			return trace.ConnectionProblem(err, "writing position during handshake: %v", err)
		}

		var err error
		peerReceivePosition, err = binary.ReadUvarint(ncReader)
		if err != nil {
			return trace.ConnectionProblem(err, "reading position during handshake: %v", err)
		}

		c.mu.Lock()
		if peerReceivePosition < c.replay.Start() || peerReceivePosition > c.replay.End() {
			// we advanced our replay buffer past the read point of the peer, or the
			// read point of the peer is in the future - can't continue, either way
			c.mu.Unlock()
			return trace.BadParameter("incompatible resume position")
		}
		if c.replay.Start() != peerReceivePosition {
			c.replay.Advance(peerReceivePosition - c.replay.Start())
			c.broadcastLocked()
		}
		c.mu.Unlock()

		logrus.Error("handshake completed successfully")
	} else {
		logrus.Error("first run, skipped handshake")
	}

	eg, ctx := errgroup.WithContext(context.Background())

	eg.Go(func() error {
		defer nc.Close()

		for {
			ack, err := binary.ReadUvarint(ncReader)
			if err != nil {
				return trace.ConnectionProblem(err, "failed reading ack: %v", err)
			}

			if ack > 0 {
				// if ack == 0xffff_ffff_ffff_ffff {
				// 	// explicit close
				// }
				c.mu.Lock()

				if replayLen := c.replay.Len(); ack > replayLen {
					// trying to move our cursor past the replay buffer?
					c.mu.Unlock()
					return trace.BadParameter("ack past end of replay buffer (got %v, len %v)", ack, replayLen)
				}
				c.replay.Advance(ack)
				c.broadcastLocked()

				c.mu.Unlock()
			}

			frameSize, err := binary.ReadUvarint(ncReader)
			if err != nil {
				return trace.ConnectionProblem(err, "failed reading frame size: %v", err)
			}

			if frameSize > maxFrameSize {
				return trace.BadParameter("oversized frame (got %v)", frameSize)
			}

			c.mu.Lock()
			for frameSize > 0 {
				for c.receive.Len() >= readBufferSize {
					if err := c.waitLockedContext(ctx); err != nil {
						c.mu.Unlock()
						return err
					}
				}

				c.receive.Reserve(min(readBufferSize-c.receive.Len(), frameSize))
				receiveTail, _ := c.receive.Free()
				receiveTail = receiveTail[:min(frameSize, len64(receiveTail))]
				c.mu.Unlock()

				n, err := io.ReadFull(ncReader, receiveTail)

				c.mu.Lock()
				if n > 0 {
					c.receive.Append(receiveTail[:n])
					frameSize -= uint64(n)
					c.broadcastLocked()
				}

				if err != nil {
					c.mu.Unlock()
					return trace.ConnectionProblem(err, "reading frame: %v", err)
				}
			}
			c.mu.Unlock()
		}
	})

	eg.Go(func() error {
		defer nc.Close()
		for {
			var sendAck uint64
			var sendBuf []byte

			c.mu.Lock()
			for {
				sendBuf = nil
				if c.replay.End() > peerReceivePosition {
					skip := peerReceivePosition - c.replay.Start()
					d1, d2 := c.replay.Data()
					if len64(d1) <= skip {
						sendBuf = d2[skip-len64(d1):]
					} else {
						sendBuf = d1[skip:]
					}
					if len(sendBuf) > maxFrameSize {
						sendBuf = sendBuf[:maxFrameSize]
					}
				}
				sendAck = c.receive.End() - sentReceivePosition
				if len(sendBuf) > 0 || sendAck > 0 {
					break
				}
				if err := c.waitLockedContext(ctx); err != nil {
					c.mu.Unlock()
					return err
				}
			}
			c.mu.Unlock()

			hdrBuf := binary.AppendUvarint(nil, sendAck)
			hdrBuf = binary.AppendUvarint(hdrBuf, len64(sendBuf))
			if _, err := nc.Write(hdrBuf); err != nil {
				return trace.ConnectionProblem(err, "writing frame header: %v", err)
			}
			if _, err := nc.Write(sendBuf); err != nil {
				return trace.ConnectionProblem(err, "writing frame data: %v", err)
			}

			c.mu.Lock()
			sentReceivePosition += sendAck
			peerReceivePosition += len64(sendBuf)
			c.mu.Unlock()
		}
	})

	err := eg.Wait()
	logrus.Infof("Detaching connection: %v", err)
	return trace.Wrap(err)
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

	c.readTimeout = nil
	if c.readTimer != nil {
		c.readTimer.Stop()
		c.readTimer = nil
	}

	c.writeTimeout = nil
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}

	if !t.IsZero() {
		d := time.Until(t)

		readTimeout := new(bool)
		c.readTimeout = readTimeout
		c.readTimer = time.AfterFunc(d, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			*readTimeout = true
			c.broadcastLocked()
		})

		writeTimeout := new(bool)
		c.writeTimeout = writeTimeout
		c.writeTimer = time.AfterFunc(d, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			*writeTimeout = true
			c.broadcastLocked()
		})
	}

	return nil
}

// SetReadDeadline implements [net.Conn].
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return net.ErrClosed
	}

	c.readTimeout = nil
	if c.readTimer != nil {
		c.readTimer.Stop()
		c.readTimer = nil
	}

	if !t.IsZero() {
		d := time.Until(t)

		readTimeout := new(bool)
		c.readTimeout = readTimeout
		c.readTimer = time.AfterFunc(d, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			*readTimeout = true
			c.broadcastLocked()
		})
	}

	return nil
}

// SetWriteDeadline implements [net.Conn].
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return net.ErrClosed
	}

	c.writeTimeout = nil
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}

	if !t.IsZero() {
		d := time.Until(t)

		writeTimeout := new(bool)
		c.writeTimeout = writeTimeout
		c.writeTimer = time.AfterFunc(d, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			*writeTimeout = true
			c.broadcastLocked()
		})
	}

	return nil
}

// Read implements [net.Conn].
func (c *Conn) Read(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for {
		if c.closed {
			return 0, net.ErrClosed
		}

		if c.readTimeout != nil && *c.readTimeout {
			return 0, os.ErrDeadlineExceeded
		}

		if len(b) == 0 {
			return 0, nil
		}

		if c.receive.Len() > 0 {
			d1, d2 := c.receive.Data()
			n := copy(b, d1)
			if len(b) > n {
				n += copy(b[n:], d2)
			}
			c.receive.Advance(uint64(n))
			c.broadcastLocked()
			return n, nil
		}

		c.waitLocked(nil)
	}
}

// Write implements [net.Conn].
func (c *Conn) Write(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, net.ErrClosed
	}

	if c.writeTimeout != nil && *c.writeTimeout {
		return 0, os.ErrDeadlineExceeded
	}

	if len(b) == 0 {
		return 0, nil
	}

	for {
		if c.replay.Len() < writeBufferSize {
			s := min(writeBufferSize-c.replay.Len(), len64(b))
			c.replay.Append(b[:s])
			b = b[s:]
			n += int(s)
			c.broadcastLocked()

			if len(b) == 0 {
				return n, nil
			}
		}

		c.waitLocked(nil)

		if c.closed {
			return n, net.ErrClosed
		}

		if c.writeTimeout != nil && *c.writeTimeout {
			return 0, os.ErrDeadlineExceeded
		}
	}
}

func len64(s []byte) uint64 {
	return uint64(len(s))
}

type window struct {
	data  []byte
	start uint64
	len   uint64
}

func (w *window) bounds() (capacity, left, right uint64) {
	capacity = len64(w.data)
	left = w.start % capacity
	right = left + w.len
	return
}

func (w *window) Start() uint64 {
	return w.start
}

func (w *window) Len() uint64 {
	return w.len
}

func (w *window) End() uint64 {
	return w.start + w.len
}

func (w *window) Data() ([]byte, []byte) {
	c, l, r := w.bounds()

	if r > c {
		return w.data[l:], w.data[:r-c]
	}
	return w.data[l:r], nil
}

func (w *window) Free() ([]byte, []byte) {
	c, l, r := w.bounds()

	if r > c {
		return w.data[r-c : l], nil
	}
	return w.data[r:], w.data[:l]
}

func (w *window) Reserve(n uint64) {
	if w.len+n <= len64(w.data) {
		return
	}

	d1, d2 := w.Data()
	c := len64(w.data) * 2
	for w.len+n > c {
		c *= 2
	}
	w.data = make([]byte, c)
	l := w.start % c
	copy(w.data[l:], d1)
	m := l + len64(d1)
	if m > c {
		m -= c
	}
	copy(w.data[m:], d2)
}

func (w *window) Append(b []byte) {
	w.Reserve(len64(b))
	f1, f2 := w.Free()
	copy(f2, b[copy(f1, b):])
	w.len += len64(b)
}

func (w *window) Advance(n uint64) {
	w.start += n
	w.len -= min(n, w.len)
}
