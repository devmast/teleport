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
	"io"
	"net"
	"sync"
	"time"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/teleport/lib/multiplexer"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"
)

const (
	sshPrefix     = "SSH-2.0-"
	clientSuffix  = "\x00teleport-resume-v0"
	clientPrelude = sshPrefix + clientSuffix

	ServerVersion = sshutils.SSHVersionPrefix + " resume-v0"
	serverPrelude = ServerVersion + "\r\n"
)

type connectionHandler interface {
	HandleConnection(net.Conn)
}

func NewResumableSSHServer(sshServer connectionHandler) *ResumableSSHServer {
	return &ResumableSSHServer{
		sshServer: sshServer,
		log:       logrus.WithField(trace.Component, "resume"),

		conns: make(map[[16]byte]*Conn),
	}
}

type ResumableSSHServer struct {
	sshServer connectionHandler
	log       logrus.FieldLogger

	mu    sync.Mutex
	conns map[[16]byte]*Conn
}

var _ connectionHandler = (*ResumableSSHServer)(nil)

func (r *ResumableSSHServer) HandleConnection(nc net.Conn) {
	// we write the server prelude, then we get ready to leave the connection to
	// the underlying SSH server (which must then send the exact same prelude)
	_, _ = nc.Write([]byte(serverPrelude))
	conn := multiplexer.NewConnWriteSkip(nc, len(serverPrelude))

	isResume, err := conn.ReadPrelude(clientPrelude)
	if err != nil {
		if !utils.IsOKNetworkError(err) {
			r.log.WithError(err).Error("Error while handling connection.")
		}
		conn.Close()
		return
	}
	if !isResume {
		r.log.Info("Handling non-resumable connection.")
		// the other party is not a resume-aware client, so we bail and give the
		// connection to the underlying SSH server
		r.sshServer.HandleConnection(conn)
		return
	}
	_, _ = conn.Write([]byte(serverPrelude)) // skipped

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

		if _, err := conn.Write(resumptionToken[:]); err != nil {
			if !utils.IsOKNetworkError(err) {
				r.log.WithError(err).Error("Error while handling connection.")
			}
			conn.Close()
			return
		}

		resumableConn := NewConn(conn.LocalAddr(), conn.RemoteAddr())
		r.mu.Lock()
		r.conns[resumptionToken] = resumableConn
		r.mu.Unlock()

		go r.sshServer.HandleConnection(resumableConn)

		<-resumableConn.Attach(conn)
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

	r.log.Info("ATTACHING CONNECTION")
	<-resumableConn.Attach(conn)
}

func NewResumableSSHClientConn(nc net.Conn, dial func(ctx context.Context) (net.Conn, error)) (net.Conn, error) {
	// we must send the first 8 bytes of the version string; thankfully, no
	// matter which SSH client we'll end up using, the handshake will almost
	// always start with `SSH-2.0-`
	//
	// TODO(espadolini): we could read the handshake from the client side first,
	// to be able to handle (without resumption support) handshakes like
	// `SSH-2.0\r\n` which is technically valid
	_, _ = nc.Write([]byte(sshPrefix))
	conn := multiplexer.NewConnWriteSkip(nc, len(sshPrefix))

	isResume, err := conn.ReadPrelude(serverPrelude)
	if err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}
	if !isResume {
		return conn, nil
	}
	_, _ = conn.Write([]byte(sshPrefix)) // skipped

	if _, err := conn.Write([]byte(clientSuffix + "\x00")); err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}

	var resumptionToken [16]byte
	if _, err := io.ReadFull(conn, resumptionToken[:]); err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}

	resumableConn := NewConn(conn.LocalAddr(), conn.RemoteAddr())
	detached := resumableConn.Attach(conn)

	go func() {
		var backoff time.Duration
		for {
			<-detached
			if closed, _ := resumableConn.Status(); closed {
				return
			}

			time.Sleep(backoff)
			backoff += 50 * time.Millisecond

			nc, err := dial(context.Background())
			if err != nil {
				logrus.Errorf("FAILED DIAL %v", err.Error())
				continue
			}

			c := multiplexer.NewConn(nc)
			if _, err := c.Write([]byte(clientPrelude)); err != nil {
				logrus.Errorf("ERROR WRITING PRELUDE %v", err.Error())
				c.Close()
				continue
			}
			isResume, err := c.ReadPrelude(serverPrelude)
			if err != nil || !isResume {
				if err != nil {
					logrus.Errorf("ERROR READING PRELUDE %v", err.Error())
				} else {
					logrus.Errorf("NOT RESUME")
				}
				c.Close()
				continue
			}
			if _, err := c.Write(resumptionToken[:]); err != nil {
				logrus.Errorf("ERROR WRITING TOKEN %v", err)
				c.Close()
				continue
			}
			logrus.Info("SUCCESSFUL RESUME; ATTACHING")

			detached = resumableConn.Attach(c)
			backoff = 0
		}
	}()

	return resumableConn, nil
}
