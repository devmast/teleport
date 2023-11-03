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
	c := &Conn{
		localAddr:  localAddr,
		remoteAddr: remoteAddr,

		receive: window{data: make([]byte, 4096)},
		replay:  window{data: make([]byte, 4096)},
	}
	c.cond.L = &c.mu
	return c
}

type Conn struct {
	mu   sync.Mutex
	cond sync.Cond

	localAddr    net.Addr
	remoteAddr   net.Addr
	allowRoaming bool

	closed bool
	// current is set iff the Conn is attached; it's cleared at the end of run()
	current io.Closer

	readDeadline  deadline
	writeDeadline deadline

	receive window
	replay  window
}

var _ net.Conn = (*Conn)(nil)

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
	c.cond.Broadcast()
	go func() {
		detached <- c.run(nc, new)
	}()

	return detached
}

func (c *Conn) detachLocked() {
	for c.current != nil {
		c.current.Close()
		c.cond.Wait()
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
		c.cond.Broadcast()
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

	varintBuf := make([]byte, 0, 2*binary.MaxVarintLen64)
	if !firstRun {
		if _, err := nc.Write(binary.AppendUvarint(varintBuf, sentReceivePosition)); err != nil {
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
			c.cond.Broadcast()
		}
		c.mu.Unlock()

		logrus.Error("handshake completed successfully")
	} else {
		logrus.Error("first run, skipped handshake")
	}

	var eg errgroup.Group
	var done bool

	eg.Go(func() error {
		defer func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if done {
				return
			}
			done = true
			c.cond.Broadcast()
		}()
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
				c.cond.Broadcast()

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
					c.cond.Wait()
					if done {
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
					c.cond.Broadcast()
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
		defer func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if done {
				return
			}
			done = true
			c.cond.Broadcast()
		}()
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
				c.cond.Wait()
				if done {
					c.mu.Unlock()
					return nil
				}
			}
			c.mu.Unlock()

			hdrBuf := binary.AppendUvarint(varintBuf, sendAck)
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
	c.cond.Broadcast()

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

// SetDeadline implements [net.Conn].
func (c *Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return net.ErrClosed
	}

	c.readDeadline.SetDeadlineLocked(t, &c.cond)
	c.writeDeadline.SetDeadlineLocked(t, &c.cond)

	return nil
}

// SetReadDeadline implements [net.Conn].
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return net.ErrClosed
	}

	c.readDeadline.SetDeadlineLocked(t, &c.cond)

	return nil
}

// SetWriteDeadline implements [net.Conn].
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return net.ErrClosed
	}

	c.writeDeadline.SetDeadlineLocked(t, &c.cond)

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

		if c.readDeadline.TimeoutLocked() {
			return 0, os.ErrDeadlineExceeded
		}

		if len(b) == 0 {
			return 0, nil
		}

		n := c.receive.Read(b)
		if n > 0 {
			c.cond.Broadcast()
			return n, nil
		}

		c.cond.Wait()
	}
}

// Write implements [net.Conn].
func (c *Conn) Write(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, net.ErrClosed
	}

	if c.writeDeadline.TimeoutLocked() {
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
			c.cond.Broadcast()

			if len(b) == 0 {
				return n, nil
			}
		}

		c.cond.Wait()

		if c.closed {
			return n, net.ErrClosed
		}

		if c.writeDeadline.TimeoutLocked() {
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

func (w *window) Read(b []byte) int {
	d1, d2 := w.Data()
	n := copy(b, d1)
	n += copy(b[n:], d2)
	w.Advance(uint64(n))
	return n
}

type deadline struct {
	timeout  bool
	deadline time.Time
	timer    *time.Timer
	stopped  bool
}

func (d *deadline) TimeoutLocked() bool {
	return d.timeout
}

func (d *deadline) SetDeadlineLocked(t time.Time, cond *sync.Cond) {
	if t.Equal(d.deadline) {
		return
	}
	d.deadline = t

	if !d.stopped && d.timer != nil {
		if !d.timer.Stop() && !d.timeout {
			// the timer has fired but the callback hasn't completed yet (it's
			// likely blocked on the lock which we're holding), so we have to
			// change d.timer to prevent it from setting a timeout that should
			// no longer be valid
			d.timer = nil
		} else {
			d.stopped = true
		}
	}

	if t.IsZero() {
		d.timeout = false
		return
	}

	dt := time.Until(t)

	if dt <= 0 {
		d.timeout = true
		cond.Broadcast()
		return
	}

	if d.timer == nil {
		thisTimer := new(*time.Timer)
		d.timer = time.AfterFunc(dt, func() {
			cond.L.Lock()
			defer cond.L.Unlock()
			if d.timer != *thisTimer {
				return
			}
			d.timeout = true
			cond.Broadcast()
		})
		*thisTimer = d.timer
	} else {
		d.timer.Reset(dt)
	}

	d.stopped = false
}
