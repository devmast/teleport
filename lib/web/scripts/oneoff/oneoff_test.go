/*
Copyright 2023 Gravitational, Inc.

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

package oneoff

import (
	"bytes"
	_ "embed"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/buildkite/bintest/v3"
	"github.com/gravitational/trace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/lib/modules"
	"github.com/gravitational/teleport/lib/utils"
)

func TestOneOffScript(t *testing.T) {
	teleportVersionOutput := "Teleport v13.1.0 git:api/v13.1.0-0-gd83ec74 go1.20.4"
	scriptName := "oneoff.sh"

	unameMock, err := bintest.NewMock("uname")
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, unameMock.Close())
	}()

	mktempMock, err := bintest.NewMock("mktemp")
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, mktempMock.Close())
	}()

	script, err := BuildScript(OneOffScriptParams{
		BinUname:        unameMock.Path,
		BinMktemp:       mktempMock.Path,
		CDNBaseURL:      "dummyURL",
		TeleportVersion: "v13.1.0",
		TeleportArgs:    "version",
	})
	require.NoError(t, err)

	t.Run("command can be executed", func(t *testing.T) {
		// set up
		testWorkingDir := t.TempDir()
		require.NoError(t, os.Mkdir(testWorkingDir+"/bin/", 0o755))
		scriptLocation := testWorkingDir + "/" + scriptName

		teleportMock, err := bintest.NewMock(testWorkingDir + "/bin/teleport")
		require.NoError(t, err)
		defer func() {
			assert.NoError(t, teleportMock.Close())
		}()

		teleportBinTarball, err := utils.CompressTarGzArchive([]string{"teleport/teleport"}, singleFileFS{file: teleportMock.Path})
		require.NoError(t, err)

		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "/teleport-v13.1.0-linux-amd64-bin.tar.gz", req.URL.Path)
			http.ServeContent(w, req, "teleport-v13.1.0-linux-amd64-bin.tar.gz", time.Now(), bytes.NewReader(teleportBinTarball.Bytes()))
		}))
		defer func() { testServer.Close() }()

		script, err := BuildScript(OneOffScriptParams{
			BinUname:        unameMock.Path,
			BinMktemp:       mktempMock.Path,
			CDNBaseURL:      testServer.URL,
			TeleportVersion: "v13.1.0",
			TeleportArgs:    "version",
			SuccessMessage:  "Test was a success.",
		})
		require.NoError(t, err)

		unameMock.Expect("-s").AndWriteToStdout("Linux")
		unameMock.Expect("-m").AndWriteToStdout("x86_64")
		mktempMock.Expect("-d").AndWriteToStdout(testWorkingDir)
		teleportMock.Expect("version").AndWriteToStdout(teleportVersionOutput)

		err = os.WriteFile(scriptLocation, []byte(script), 0700)
		require.NoError(t, err)

		// execute script
		out, err := exec.Command("bash", scriptLocation).CombinedOutput()

		// validate
		require.NoError(t, err, string(out))

		require.True(t, unameMock.Check(t))
		require.True(t, mktempMock.Check(t))
		require.True(t, teleportMock.Check(t))

		require.Contains(t, string(out), "> ./bin/teleport version")
		require.Contains(t, string(out), teleportVersionOutput)
		require.Contains(t, string(out), "Test was a success.")
	})

	t.Run("invalid OS", func(t *testing.T) {
		// set up
		testWorkingDir := t.TempDir()
		scriptLocation := testWorkingDir + "/" + scriptName

		unameMock.Expect("-s").AndWriteToStdout("Windows")
		unameMock.Expect("-m").AndWriteToStdout("x86_64")
		mktempMock.Expect("-d").AndWriteToStdout(testWorkingDir)

		err = os.WriteFile(scriptLocation, []byte(script), 0700)
		require.NoError(t, err)

		// execute script
		out, err := exec.Command("bash", scriptLocation).CombinedOutput()

		// validate
		require.Error(t, err, string(out))
		require.Contains(t, string(out), "Only MacOS and Linux are supported.")
	})

	t.Run("invalid Arch", func(t *testing.T) {
		// set up
		testWorkingDir := t.TempDir()
		scriptLocation := testWorkingDir + "/" + scriptName

		unameMock.Expect("-s").AndWriteToStdout("Linux")
		unameMock.Expect("-m").AndWriteToStdout("apple-silicon")
		mktempMock.Expect("-d").AndWriteToStdout(testWorkingDir)

		err = os.WriteFile(scriptLocation, []byte(script), 0700)
		require.NoError(t, err)

		// execute script
		out, err := exec.Command("bash", scriptLocation).CombinedOutput()

		// validate
		require.Error(t, err, string(out))
		require.Contains(t, string(out), "Invalid Linux architecture apple-silicon.")
	})

	t.Run("invalid flavor should return an error", func(t *testing.T) {
		_, err := BuildScript(OneOffScriptParams{
			BinUname:        unameMock.Path,
			BinMktemp:       mktempMock.Path,
			CDNBaseURL:      "dummyURL",
			TeleportVersion: "v13.1.0",
			TeleportArgs:    "version",
			SuccessMessage:  "Test was a success.",
			TeleportFlavor:  "../not-teleport",
		})
		require.True(t, trace.IsBadParameter(err), "expected BadParameter, got %+v", err)
	})

	t.Run("if enterprise build, it uses the enterprise package name", func(t *testing.T) {
		// set up
		testWorkingDir := t.TempDir()
		require.NoError(t, os.Mkdir(testWorkingDir+"/bin/", 0o755))
		scriptLocation := testWorkingDir + "/" + scriptName

		teleportMock, err := bintest.NewMock(testWorkingDir + "/bin/teleport")
		require.NoError(t, err)
		defer func() {
			assert.NoError(t, teleportMock.Close())
		}()

		modules.SetTestModules(t, &modules.TestModules{
			TestBuildType: modules.BuildEnterprise,
		})
		teleportBinTarball, err := utils.CompressTarGzArchive([]string{"teleport-ent/teleport"}, singleFileFS{file: teleportMock.Path})
		require.NoError(t, err)

		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "/teleport-ent-v13.1.0-linux-amd64-bin.tar.gz", req.URL.Path)
			http.ServeContent(w, req, "teleport-ent-v13.1.0-linux-amd64-bin.tar.gz", time.Now(), bytes.NewReader(teleportBinTarball.Bytes()))
		}))
		defer func() { testServer.Close() }()

		script, err := BuildScript(OneOffScriptParams{
			BinUname:        unameMock.Path,
			BinMktemp:       mktempMock.Path,
			CDNBaseURL:      testServer.URL,
			TeleportVersion: "v13.1.0",
			TeleportArgs:    "version",
			SuccessMessage:  "Test was a success.",
		})
		require.NoError(t, err)

		unameMock.Expect("-s").AndWriteToStdout("Linux")
		unameMock.Expect("-m").AndWriteToStdout("x86_64")
		mktempMock.Expect("-d").AndWriteToStdout(testWorkingDir)
		teleportMock.Expect("version").AndWriteToStdout(teleportVersionOutput)

		err = os.WriteFile(scriptLocation, []byte(script), 0700)
		require.NoError(t, err)

		// execute script
		out, err := exec.Command("bash", scriptLocation).CombinedOutput()

		// validate
		require.NoError(t, err, string(out))

		require.True(t, unameMock.Check(t))
		require.True(t, mktempMock.Check(t))
		require.True(t, teleportMock.Check(t))

		require.Contains(t, string(out), "> ./bin/teleport version")
		require.Contains(t, string(out), teleportVersionOutput)
		require.Contains(t, string(out), "Test was a success.")
	})
}

type singleFileFS struct {
	file string
}

func (m singleFileFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(m.file)
}

func (m singleFileFS) Open(name string) (fs.File, error) {
	return os.Open(m.file)
}

func (m singleFileFS) Stat(name string) (fs.FileInfo, error) {
	return os.Stat(m.file)
}
