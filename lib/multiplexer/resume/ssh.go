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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/teleport/lib/multiplexer"
	"github.com/gravitational/teleport/lib/utils"
)

const (
	protocolString = "teleport-resume-v0"

	sshPrefix     = "SSH-2.0-"
	clientSuffix  = "\x00" + protocolString
	clientPrelude = sshPrefix + clientSuffix

	maxU64Bytes = "\xff\xff\xff\xff\xff\xff\xff\xff"
	maxU64      = 0xffff_ffff_ffff_ffff
)

type connectionHandler interface {
	HandleConnection(net.Conn)
}

func NewResumableSSHServer(sshServer connectionHandler, serverVersion, hostID string) *ResumableSSHServer {
	return &ResumableSSHServer{
		sshServer: sshServer,

		serverVersion: serverVersion,
		hostID:        hostID,

		log: logrus.WithField(trace.Component, "resume"),

		conns: make(map[token]*Conn),
	}
}

type token = [8]byte

func newToken() token {
	var t token
	if _, err := rand.Read(t[:]); err != nil {
		panic(err)
	}
	return t
}

type ResumableSSHServer struct {
	sshServer connectionHandler
	log       logrus.FieldLogger

	serverVersion string
	hostID        string

	mu    sync.Mutex
	conns map[token]*Conn
}

var _ connectionHandler = (*ResumableSSHServer)(nil)

func multiplexerConn(nc net.Conn) *multiplexer.Conn {
	if conn, ok := nc.(*multiplexer.Conn); ok {
		return conn
	}
	return multiplexer.NewConn(nc)
}

func (r *ResumableSSHServer) HandleConnection(nc net.Conn) {
	thisToken := newToken()

	conn := multiplexerConn(nc)
	if _, err := conn.Write([]byte(fmt.Sprintf("%v %x %v\r\n%v\r\n", protocolString, thisToken, r.hostID, r.serverVersion))); err != nil {
		if !utils.IsOKNetworkError(err) {
			r.log.WithError(err).Error("Error while writing resumption prelude.")
		}
		conn.Close()
		return
	}

	isResume, err := conn.ReadPrelude(clientPrelude)
	if err != nil {
		if !utils.IsOKNetworkError(err) {
			r.log.WithError(err).Error("Error while reading resumption prelude.")
		}
		conn.Close()
		return
	}

	if !isResume {
		r.log.Info("Handling non-resumable connection.")
		r.sshServer.HandleConnection(newWriteSkipConn(conn, uint32(len(r.serverVersion)+len("\r\n"))))
		return
	}

	var resumptionToken token
	if _, err := io.ReadFull(conn, resumptionToken[:]); err != nil {
		if !utils.IsOKNetworkError(err) {
			r.log.WithError(err).Error("Error while reading resumption handshake.")
		}
		conn.Close()
		return
	}

	if resumptionToken == thisToken {
		r.log.Info("Handling new resumable SSH connection.")

		resumableConn := NewConn(conn.LocalAddr(), conn.RemoteAddr(), nil, uint64(len(r.serverVersion)+len("\r\n")))
		r.mu.Lock()
		r.conns[thisToken] = resumableConn
		r.mu.Unlock()

		go r.sshServer.HandleConnection(resumableConn)
		r.log.Debugf("Handling new resumable connection: %v", resumableConn.Attach(conn, true))
		return
	}

	r.log.Info("===== REATTACHING CONNECTION ======")

	r.mu.Lock()
	resumableConn := r.conns[resumptionToken]
	r.mu.Unlock()
	if resumableConn == nil {
		// sentinel value for the handshake to signify "unknown resumption token
		// or invalid resumption point"
		_, _ = conn.Write([]byte(maxU64Bytes))
		conn.Close()
		r.log.Info("===== UNKNOWN RESUMPTION TOKEN =====")
		return
	}

	r.log.Debugf("Handling existing resumable connection: %v", resumableConn.Attach(conn, false))
}

var resumablePreludeLine = regexp.MustCompile(`^` + regexp.QuoteMeta(protocolString) + ` ([0-9a-f]{16}) ([0-9a-z\-]+)\r\n$`)

// readVersionExchange will read LF-terminated lines from the conn until it
// either finds a SSH- one, which indicates the end of the SSH version exchange,
// or it finds a match for a resumption protocol version line, from which it'll
// extract the included values, and it will then peek the next line as the early
// data for the resumable connection. Since the LF-delimited early data is a
// requirement of the protocol, the version exchange indicates a success with a
// non-empty earlyData return value.
func readVersionExchange(conn *multiplexer.Conn) (resumptionToken token, hostID string, earlyData []byte, err error) {
	// as per RFC 4253 section 4.2, the maximum amount of data in the version
	// exchange is 255 bytes
	maxLength := 255

	for {
		var line []byte
		line, err = conn.PeekLine(maxLength)
		if err != nil {
			return
		}
		maxLength -= len(line)

		if bytes.HasPrefix(line, []byte("SSH-")) {
			// we got to the (final) SSH line without encountering the
			// resumption line, we return with an empty earlyData to signify
			// that the connection is normal
			return
		}

		match := resumablePreludeLine.FindSubmatch(line)
		if match == nil {
			// discard is guaranteed to work for the line we just peeked
			_, _ = conn.Discard(len(line))
			continue
		}

		// the regexp guarantees that we have an even number of hex characters
		_, _ = hex.Decode(resumptionToken[:], match[1])
		hostID = string(match[2])

		// discard is guaranteed to work for the line we just peeked
		_, _ = conn.Discard(len(line))
		break
	}

	earlyData, err = conn.PeekLine(maxLength)
	return
}

func MaybeResumableSSHClientConn(nc net.Conn, connCtx context.Context, dial func(connCtx context.Context, hostID string) (net.Conn, error)) (net.Conn, error) {
	conn := multiplexerConn(nc)

	// we must send the first 8 bytes of the version string; thankfully, no
	// matter which SSH client we'll end up using, it must send `SSH-2.0-` as
	// its first 8 bytes, as per RFC 4253 ("The Secure Shell (SSH) Transport
	// Layer Protocol") section 4.2.
	if _, err := conn.Write([]byte(sshPrefix)); err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}

	resumptionToken, hostID, earlyData, err := readVersionExchange(conn)
	if err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}
	if len(earlyData) == 0 {
		// regular SSH connection, conn is about to read the SSH- line from the
		// server but we've sent sshPrefix already, so we have to skip it from
		// the application side writes
		return newWriteSkipConn(conn, uint32(len(sshPrefix))), nil
	}

	if _, err := conn.Write([]byte(clientSuffix)); err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}

	if _, err := conn.Write(resumptionToken[:]); err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}

	resumableConn := NewConn(conn.LocalAddr(), conn.RemoteAddr(), earlyData, 0)
	_, _ = conn.Discard(len(earlyData))
	resumableConn.AllowRoaming()

	detached := make(chan struct{})
	go func() {
		defer close(detached)
		resumableConn.Attach(conn, true)
	}()

	go func() {
		const replacementInterval = 30 * time.Second
		reconnectTicker := time.NewTicker(replacementInterval)
		defer reconnectTicker.Stop()

		var backoff time.Duration
		for {
			select {
			case <-connCtx.Done():
				resumableConn.Close()
				return
			case <-detached:
			case <-reconnectTicker.C:
			}

			closed, attached := resumableConn.Status()
			if closed {
				return
			}

			if attached {
				logrus.Debug("Attempting periodic connection replacement.")
			} else {
				logrus.Debugf("Connection lost, reconnecting after %v.", backoff)
				time.Sleep(backoff)
				backoff += 500 * time.Millisecond
			}

			if connCtx.Err() != nil {
				return
			}
			logrus.Debug("Dialing.")
			nc, err := dial(connCtx, hostID)
			if err != nil {
				logrus.Errorf("Failed to dial: %v.", err.Error())
				continue
			}

			c := multiplexerConn(nc)
			if _, err := c.Write([]byte(clientPrelude)); err != nil {
				logrus.Errorf("Error writing resumption prelude: %v.", err.Error())
				c.Close()
				continue
			}
			if _, err := c.Write(resumptionToken[:]); err != nil {
				logrus.Errorf("Error writing resumption token: %v.", err)
				c.Close()
				continue
			}

			_, _, earlyData, err := readVersionExchange(c)
			if err != nil {
				logrus.Errorf("Error reading resumption version exchange: %v.", err)
				c.Close()
				continue
			}
			if len(earlyData) == 0 {
				logrus.Errorf("Somehow not a resumable connection on resumption.")
				c.Close()
				continue
			}
			_, _ = c.Discard(len(earlyData))

			if fail, err := c.ReadPrelude(maxU64Bytes); err != nil {
				logrus.Errorf("Error receiving resumption handshake: %v.", err)
				c.Close()
				continue
			} else if fail {
				if !attached {
					logrus.Errorf("Failure to resume connection.")
					c.Close()
					resumableConn.Close()
					return
				} else {
					logrus.Errorf("Failure to replace connection.")
					c.Close()
					continue
				}
			}

			logrus.Info("Attaching connection.")
			detached = make(chan struct{})
			go func() {
				defer close(detached)
				resumableConn.Attach(c, false)
			}()

			backoff = 0
			reconnectTicker.Reset(replacementInterval)
			select {
			case <-reconnectTicker.C:
			default:
			}
		}
	}()

	return resumableConn, nil
}
