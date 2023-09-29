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

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/teleport/lib/multiplexer"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"
)

const (
	sshPrefix     = "SSH-2.0-"
	clientSuffix  = "\x00teleport-resume-v1"
	clientPrelude = sshPrefix + clientSuffix

	ServerVersion = sshutils.SSHVersionPrefix + " resume-v1"
	serverPrelude = ServerVersion + "\r\n"
)

type connectionHandler interface {
	HandleConnection(net.Conn)
}

func NewResumableSSHServer(sshServer connectionHandler) *ResumableSSHServer {
	return &ResumableSSHServer{
		sshServer: sshServer,

		log: logrus.WithField(trace.Component, "resume"),
	}
}

type ResumableSSHServer struct {
	sshServer connectionHandler

	log logrus.FieldLogger
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
		// the other party is not a resume-aware client, so we bail and give the
		// connection to the underlying SSH server
		r.sshServer.HandleConnection(conn)
		return
	}
	_, _ = conn.Write([]byte(serverPrelude)) // skipped

	r.log.Debug("Handling resumable SSH connection.")

	// TODO(espadolini): run resumable protocol
	r.sshServer.HandleConnection(conn)
}

func NewResumableSSHClientConn(nc net.Conn) (net.Conn, error) {
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

	if _, err := conn.Write([]byte(clientSuffix)); err != nil {
		conn.Close()
		return nil, trace.Wrap(err)
	}

	// TODO(espadolini): run resumable protocol
	return conn, nil
}
