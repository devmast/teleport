/*
Copyright 2017-2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package multiplexer

import (
	"bufio"
	"context"
	"net"
	"sync"
	"sync/atomic"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/lib/utils"
)

// Conn is a connection wrapper that supports
// communicating remote address from proxy protocol
// and replays first several bytes read during
// protocol detection
type Conn struct {
	net.Conn
	protocol  Protocol
	proxyLine *ProxyLine
	reader    *bufio.Reader

	// writeMu protects against concurrent Write calls while writeSkip is nonzero.
	writeMu sync.Mutex
	// writeSkip contains how many bytes we still have to skip while writing.
	writeSkip atomic.Uint32
}

// NewConn returns a net.Conn wrapper that supports peeking into the connection.
func NewConn(conn net.Conn) *Conn {
	return &Conn{
		Conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

// NewConnWithWriteSkip returns a net.Conn wrapper that supports peeking into
// the connection, skipping a certain amount of bytes on write (useful if
// specific bytes have been already sent but the application using the Conn
// shouldn't be made aware of it).
func NewConnWithWriteSkip(conn net.Conn, writeSkip uint32) *Conn {
	c := NewConn(conn)
	c.writeSkip.Store(writeSkip)
	return c
}

// NetConn returns the underlying net.Conn.
func (c *Conn) NetConn() net.Conn {
	return c.Conn
}

// Read reads from connection
func (c *Conn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *Conn) Write(p []byte) (int, error) {
	// fast path without locking; as soon as writeSkip is confirmed to be zero,
	// all bets are off about concurrent Write calls
	if c.writeSkip.Load() == 0 {
		return c.Conn.Write(p)
	}

	c.writeMu.Lock()
	writeSkip := c.writeSkip.Load()
	if writeSkip == 0 {
		// writeSkip became zero while we were blocked on writeMu, no need to
		// serialize writes anymore
		c.writeMu.Unlock()
		return c.Conn.Write(p)
	}
	defer c.writeMu.Unlock()

	if uint64(len(p)) <= uint64(writeSkip) {
		// still check for a write deadline or other errors
		if _, err := c.Conn.Write(nil); err != nil {
			return 0, trace.Wrap(err)
		}
		c.writeSkip.Store(writeSkip - uint32(len(p)))
		return len(p), nil
	}

	n, err := c.Conn.Write(p[writeSkip:])
	if n > 0 || err == nil {
		n += int(writeSkip)
		c.writeSkip.Store(0)
	}

	return n, trace.Wrap(err)
}

// LocalAddr returns local address of the connection
func (c *Conn) LocalAddr() net.Addr {
	if c.proxyLine != nil {
		return &c.proxyLine.Destination
	}
	return c.Conn.LocalAddr()
}

// RemoteAddr returns remote address of the connection
func (c *Conn) RemoteAddr() net.Addr {
	if c.proxyLine != nil {
		return &c.proxyLine.Source
	}
	return c.Conn.RemoteAddr()
}

// Protocol returns the detected connection protocol
func (c *Conn) Protocol() Protocol {
	return c.protocol
}

// Detect detects the connection protocol by peeking into the first few bytes.
func (c *Conn) Detect() (Protocol, error) {
	proto, err := detectProto(c.reader)
	if err != nil && !trace.IsBadParameter(err) {
		return ProtoUnknown, trace.Wrap(err)
	}
	return proto, nil
}

// ReadProxyLine reads proxy-line from the connection.
func (c *Conn) ReadProxyLine() (*ProxyLine, error) {
	var proxyLine *ProxyLine
	protocol, err := c.Detect()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if protocol == ProtoProxyV2 {
		proxyLine, err = ReadProxyLineV2(c.reader)
	} else {
		proxyLine, err = ReadProxyLine(c.reader)
	}
	if err != nil {
		return nil, trace.Wrap(err)
	}
	c.proxyLine = proxyLine
	return proxyLine, nil
}

// ReadPrelude reads the specified prelude from the connection, returning true
// if it succeeds; if the function returns false, no byte has been read and the
// next bytes to be read will not match the prelude.
func (c *Conn) ReadPrelude(prelude string) (bool, error) {
	for i, b := range []byte(prelude) {
		buf, err := c.reader.Peek(i + 1)
		if err != nil {
			return false, trace.Wrap(err)
		}

		if buf[i] != b {
			return false, nil
		}
	}

	// Discard is guaranteed to succeed if enough bytes are buffered, and we
	// have peeked the whole prelude
	_, _ = c.reader.Discard(len(prelude))
	return true, nil
}

// returns a Listener that pretends to be listening on addr, closed whenever the
// parent context is done.
func newListener(parent context.Context, addr net.Addr) *Listener {
	context, cancel := context.WithCancel(parent)
	return &Listener{
		addr:    addr,
		connC:   make(chan net.Conn),
		cancel:  cancel,
		context: context,
	}
}

// Listener is a listener that receives
// connections from multiplexer based on the connection type
type Listener struct {
	addr    net.Addr
	connC   chan net.Conn
	cancel  context.CancelFunc
	context context.Context
}

// Addr returns listener addr, the address of multiplexer listener
func (l *Listener) Addr() net.Addr {
	return l.addr
}

// Accept accepts connections from parent multiplexer listener
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case <-l.context.Done():
		return nil, trace.ConnectionProblem(net.ErrClosed, "listener is closed")
	case conn := <-l.connC:
		return conn, nil
	}
}

// HandleConnection injects the connection into the Listener, blocking until the
// context expires, the connection is accepted or the Listener is closed.
func (l *Listener) HandleConnection(ctx context.Context, conn net.Conn) {
	select {
	case <-ctx.Done():
		conn.Close()
	case <-l.context.Done():
		conn.Close()
	case l.connC <- conn:
	}
}

// Close closes the listener.
func (l *Listener) Close() error {
	l.cancel()
	return nil
}

// PROXYEnabledListener wraps provided listener and can receive and apply PROXY headers and then pass connection up the chain.
type PROXYEnabledListener struct {
	cfg Config
	mux *Mux
	net.Listener
}

// NewPROXYEnabledListener creates news instance of PROXYEnabledListener
func NewPROXYEnabledListener(cfg Config) (*PROXYEnabledListener, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	mux, err := New(cfg) // Creating Mux to leverage protocol detection with PROXY headers
	if err != nil {
		return nil, trace.Wrap(err)
	}
	muxListener := mux.SSH()
	go func() {
		if err := mux.Serve(); err != nil && !utils.IsOKNetworkError(err) {
			mux.Entry.WithError(err).Error("Mux encountered err serving")
		}
	}()
	pl := &PROXYEnabledListener{
		cfg:      cfg,
		mux:      mux,
		Listener: muxListener,
	}

	return pl, nil
}

func (p *PROXYEnabledListener) Close() error {
	return trace.Wrap(p.mux.Close())
}

// Accept gets connection from the wrapped listener and detects whether we receive PROXY headers on it,
// after first non PROXY protocol detected it returns connection with PROXY addresses applied to it.
func (p *PROXYEnabledListener) Accept() (net.Conn, error) {
	conn, err := p.Listener.Accept()
	return conn, trace.Wrap(err)
}
