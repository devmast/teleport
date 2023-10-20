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
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
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

	serverPrelude = protocolString + "\r\n"
)

type connectionHandler interface {
	HandleConnection(net.Conn)
}

func NewResumableSSHServer(sshServer connectionHandler, hostID string) *ResumableSSHServer {
	hostIDBuf := make([]byte, 0, 8+len(hostID))
	hostIDBuf = binary.LittleEndian.AppendUint64(hostIDBuf, uint64(len(hostID)))
	hostIDBuf = append(hostIDBuf, hostID...)

	return &ResumableSSHServer{
		sshServer: sshServer,
		hostIDBuf: hostIDBuf,
		log:       logrus.WithField(trace.Component, "resume"),

		conns: make(map[[16]byte]*Conn),
	}
}

type ResumableSSHServer struct {
	sshServer connectionHandler
	hostIDBuf []byte
	log       logrus.FieldLogger

	mu    sync.Mutex
	conns map[[16]byte]*Conn
}

var _ connectionHandler = (*ResumableSSHServer)(nil)

func multiplexerConn(nc net.Conn) *multiplexer.Conn {
	if conn, ok := nc.(*multiplexer.Conn); ok {
		return conn
	}
	return multiplexer.NewConn(nc)
}

func (r *ResumableSSHServer) HandleConnection(nc net.Conn) {
	conn := multiplexerConn(nc)
	if _, err := conn.Write([]byte(serverPrelude)); err != nil {
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
		// the other party is not a resume-aware client, so we bail and give the
		// connection to the underlying SSH server; we have written a
		// CRLF-terminated line and read nothing from the connection, so it's
		// legal for a SSH server to take over the connection from here
		r.sshServer.HandleConnection(conn)
		return
	}

	isNew, err := conn.ReadPrelude("\x00")
	if err != nil {
		if !utils.IsOKNetworkError(err) {
			r.log.WithError(err).Error("Error while handling connection.")
		}
		conn.Close()
		return
	}
	if isNew {
		r.log.Info("Handling new resumable SSH connection.")

		var resumptionToken [16]byte
		if _, err := rand.Read(resumptionToken[:]); err != nil {
			r.log.WithError(err).Error("Failed to generate resumption token.")
			conn.Close()
			return
		}
		resumptionToken[0] |= 0x80

		if _, err := conn.Write(append(resumptionToken[:], r.hostIDBuf...)); err != nil {
			if !utils.IsOKNetworkError(err) {
				r.log.WithError(err).Error("Error during resumption handshake.")
			}
			conn.Close()
			return
		}

		resumableConn := NewConn(conn.LocalAddr(), conn.RemoteAddr())
		r.mu.Lock()
		r.conns[resumptionToken] = resumableConn
		r.mu.Unlock()

		go r.sshServer.HandleConnection(resumableConn)
		<-resumableConn.Attach(conn, true)
		return
	}

	r.log.Info("===== REATTACHING CONNECTION ======")
	var resumptionToken [16]byte
	if _, err := io.ReadFull(conn, resumptionToken[:]); err != nil {
		r.log.WithError(err).Error("===== FAILED TO READ RESUMPTION TOKEN ======")
		conn.Close()
		return
	}

	r.mu.Lock()
	resumableConn := r.conns[resumptionToken]
	r.mu.Unlock()
	if resumableConn == nil {
		r.log.Error("====== CONNECTION NOT FOUND ======")
		conn.Close()
		return
	}

	conn.Write([]byte("\x01"))
	r.log.Info("ATTACHING CONNECTION")
	<-resumableConn.Attach(conn, false)
}

func NewResumableSSHClientConn(nc net.Conn, connCtx context.Context, dial func(connCtx context.Context, addrPort string) (net.Conn, error)) (net.Conn, error) {
	// we must send the first 8 bytes of the version string; thankfully, no
	// matter which SSH client we'll end up using, it must send `SSH-2.0-` as
	// its first 8 bytes, as per RFC 4253 ("The Secure Shell (SSH) Transport
	// Layer Protocol") section 4.2.
	if _, err := nc.Write([]byte(sshPrefix)); err != nil {
		nc.Close()
		return nil, trace.Wrap(err)
	}
	conn := multiplexer.NewConn(nc)

	isResume, err := conn.ReadPrelude(serverPrelude)
	if err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}
	if !isResume {
		// the server side doesn't support resumption, so we just return the
		// conn after having written sshPrefix to it already - we are going to
		// assume that the application side will write the sshPrefix to it
		// again, so we skip it
		return newWriteSkipConn(conn, uint32(len(sshPrefix))), nil
	}

	if _, err := conn.Write([]byte(clientSuffix + "\x00")); err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}

	var resumptionToken [16]byte
	if _, err := io.ReadFull(conn, resumptionToken[:]); err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}

	var hostIDPort string
	{
		var hostIDLenBuf [8]byte
		if _, err := io.ReadFull(conn, hostIDLenBuf[:]); err != nil {
			conn.Close()
			return nil, trace.Wrap(err)
		}
		hostIDLen := binary.LittleEndian.Uint64(hostIDLenBuf[:])
		if hostIDLen > 256 {
			conn.Close()
			return nil, trace.BadParameter("overlong hostID %v", hostIDLen)
		}
		hostIDBuf := make([]byte, hostIDLen)
		if _, err := io.ReadFull(conn, hostIDBuf); err != nil {
			conn.Close()
			return nil, trace.Wrap(err)
		}
		hostIDPort = string(hostIDBuf) + ":0"
	}

	resumableConn := NewConn(conn.LocalAddr(), conn.RemoteAddr())
	resumableConn.AllowRoaming()
	detached := resumableConn.Attach(conn, true)

	go func() {
		reconnectTicker := time.NewTicker(30 * time.Second)
		defer reconnectTicker.Stop()

		var backoff time.Duration
		for {
			select {
			case <-detached:
			case <-connCtx.Done():
				resumableConn.Close()
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
			nc, err := dial(connCtx, hostIDPort)
			if err != nil {
				logrus.Errorf("Failed to dial: %v.", err.Error())
				continue
			}

			c := multiplexer.NewConn(nc)
			if _, err := c.Write([]byte(clientPrelude)); err != nil {
				logrus.Errorf("Error writing resumption prelude: %v.", err.Error())
				c.Close()
				continue
			}
			isResume, err := c.ReadPrelude(serverPrelude)
			if err != nil || !isResume {
				if err != nil {
					logrus.Errorf("Error reading resumption prelude: %v.", err.Error())
				} else {
					logrus.Errorf("Error reading resumption prelude: server is somehow not resumable.")
				}
				c.Close()
				continue
			}
			if _, err := c.Write(resumptionToken[:]); err != nil {
				logrus.Errorf("Error writing resumption token: %v.", err)
				c.Close()
				continue
			}

			success, err := c.ReadPrelude("\x01")
			if err != nil || !success {
				if err != nil {
					logrus.Errorf("Error reading confirmation: %v.", err)
				} else {
					logrus.Errorf("Error reading confirmation: connection not found.")
				}
				c.Close()
				continue
			}

			logrus.Info("Attaching connection.")
			detached = resumableConn.Attach(c, false)
			logrus.Info("Connection attached.")
			backoff = 0
		}
	}()

	return resumableConn, nil
}
