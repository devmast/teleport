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

package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/lib/utils"
)

func TestOutput(t *testing.T) {
	err := errors.New("you can't do that")
	addr := utils.NetAddr{Addr: "127.0.0.1:1234", AddrNetwork: "tcp"}

	fields := logrus.Fields{
		"local":        &addr,
		"remote":       &addr,
		"login":        "llama",
		"teleportUser": "user",
		"id":           1234,
	}

	t.Run("text", func(t *testing.T) {
		var logrusOutput bytes.Buffer
		formatter := NewDefaultTextFormatter(true)
		formatter.timestampEnabled = true
		logger := logrus.New()
		logger.SetFormatter(formatter)
		logger.SetOutput(&logrusOutput)
		logger.ReplaceHooks(logrus.LevelHooks{})

		entry := logger.WithField(trace.Component, "test")
		l := entry.WithField("test", 123).WithField("animal", "llama\n").WithField("error", err)
		l.WithField("diag_addr", &addr).WithField(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers.\t To connect with profiler, use `go tool pprof diag_addr`.")

		var slogOutput bytes.Buffer
		slogLogger := slog.New(NewSLogTextHandler(&slogOutput, slog.LevelDebug, true)).With(trace.Component, "test")
		l2 := slogLogger.With("test", 123).With("animal", "llama\n").With("error", err)
		l2.With(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers.\t To connect with profiler, use `go tool pprof diag_addr`.", "diag_addr", &addr)

		require.Equal(t, logrusOutput.String(), slogOutput.String())
	})

	t.Run("json", func(t *testing.T) {
		var logrusOut bytes.Buffer
		formatter := &JSONFormatter{
			ExtraFields:   nil,
			callerEnabled: true,
		}

		require.NoError(t, formatter.CheckAndSetDefaults())

		logger := logrus.New()
		logger.SetFormatter(formatter)
		logger.SetOutput(&logrusOut)
		logger.ReplaceHooks(logrus.LevelHooks{})

		entry := logger.WithField(trace.Component, "test")
		l := entry.WithField("test", 123).WithField("animal", "llama").WithField("error", err)
		l.WithField("diag_addr", &addr).Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`.")

		var slogOutput bytes.Buffer
		slogLogger := slog.New(NewSlogJSONHandler(&slogOutput, slog.LevelDebug)).With(trace.Component, "test")
		l2 := slogLogger.With("test", 123).With("animal", "llama").With("error", err)
		l2.Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`.", "diag_addr", &addr)

		var logrusData map[string]any
		require.NoError(t, json.Unmarshal(logrusOut.Bytes(), &logrusData), "invalid logrus output format")

		var slogData map[string]any
		require.NoError(t, json.Unmarshal(slogOutput.Bytes(), &slogData), "invalid slog output format")

		logrusCaller, ok := logrusData["caller"].(string)
		delete(logrusData, "caller")
		assert.True(t, ok, "caller was missing from logrus output")

		slogCaller, ok := slogData["caller"].(string)
		delete(slogData, "caller")
		assert.True(t, ok, "caller was missing from slog output")

		assert.Equal(t, logrusCaller[:len(logrusCaller)-3], slogCaller[:len(slogCaller)-3])

		require.Empty(t,
			cmp.Diff(
				logrusData,
				slogData,
				cmpopts.SortMaps(func(a, b string) bool { return a < b }),
			),
		)
	})

}

func BenchmarkFormatter(b *testing.B) {
	b.ReportAllocs()
	err := errors.New("the quick brown fox jumped really high")
	//err := trace.AccessDenied("the quick brown fox jumped really high")
	addr := utils.NetAddr{Addr: "127.0.0.1:1234", AddrNetwork: "tcp"}

	fields := logrus.Fields{
		"local":        addr,
		"remote":       addr,
		"login":        "llama",
		"teleportUser": "user",
		"id":           1234,
	}

	b.ResetTimer()
	b.Run("logrus", func(b *testing.B) {
		b.Run("text_old_formatter", func(b *testing.B) {
			formatter := utils.NewDefaultTextFormatter(true)
			require.NoError(b, formatter.CheckAndSetDefaults())
			logger := logrus.New()
			logger.SetFormatter(formatter)
			logger.SetOutput(io.Discard)
			logger.ReplaceHooks(logrus.LevelHooks{})
			b.ResetTimer()

			entry := logger.WithField(trace.Component, "test")
			for i := 0; i < b.N; i++ {
				l := entry.WithField("test", 123).WithField("animal", "llama\n").WithField("error", err)
				l.WithField("diag_addr", &addr).WithField(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`\n.")
			}
		})

		b.Run("text", func(b *testing.B) {
			formatter := NewDefaultTextFormatter(true)
			require.NoError(b, formatter.CheckAndSetDefaults())
			logger := logrus.New()
			logger.SetFormatter(formatter)
			logger.SetOutput(io.Discard)
			logger.ReplaceHooks(logrus.LevelHooks{})
			b.ResetTimer()

			entry := logger.WithField(trace.Component, "test")
			for i := 0; i < b.N; i++ {
				l := entry.WithField("test", 123).WithField("animal", "llama\n").WithField("error", err)
				l.WithField("diag_addr", &addr).WithField(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`\n.")
			}
		})

		b.Run("json_old_formatter", func(b *testing.B) {
			formatter := &utils.JSONFormatter{}
			require.NoError(b, formatter.CheckAndSetDefaults())
			logger := logrus.New()
			logger.SetFormatter(formatter)
			logger.SetOutput(io.Discard)
			logger.ReplaceHooks(logrus.LevelHooks{})
			b.ResetTimer()

			entry := logger.WithField(trace.Component, "test")
			for i := 0; i < b.N; i++ {
				l := entry.WithField("test", 123).WithField("animal", "llama\n").WithField("error", err)
				l.WithField("diag_addr", &addr).WithField(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`\n.")
			}
		})

		b.Run("json", func(b *testing.B) {
			formatter := &JSONFormatter{}
			require.NoError(b, formatter.CheckAndSetDefaults())
			logger := logrus.New()
			logger.SetFormatter(formatter)
			logger.SetOutput(io.Discard)
			logger.ReplaceHooks(logrus.LevelHooks{})
			b.ResetTimer()

			entry := logger.WithField(trace.Component, "test")
			for i := 0; i < b.N; i++ {
				l := entry.WithField("test", 123).WithField("animal", "llama\n").WithField("error", err)
				l.WithField("diag_addr", &addr).WithField(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`\n.")
			}
		})
	})

	b.Run("slog", func(b *testing.B) {
		b.Run("default_text", func(b *testing.B) {
			logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
				AddSource:   true,
				Level:       slog.LevelDebug,
				ReplaceAttr: nil,
			})).With(trace.Component, "test")
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				l := logger.With("test", 123).With("animal", "llama\n").With("error", err)
				l.With(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`\n.", "diag_addr", &addr)
			}
		})

		b.Run("text", func(b *testing.B) {
			logger := slog.New(NewSLogTextHandler(io.Discard, slog.LevelDebug, true)).With(trace.Component, "test")
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				l := logger.With("test", 123).With("animal", "llama\n").With("error", err)
				l.With(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`\n.", "diag_addr", &addr)
			}
		})

		b.Run("default_json", func(b *testing.B) {
			logger := slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{
				AddSource:   true,
				Level:       slog.LevelDebug,
				ReplaceAttr: nil,
			})).With(trace.Component, "test")
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				l := logger.With("test", 123).With("animal", "llama\n").With("error", err)
				l.With(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`\n.", "diag_addr", &addr)
			}
		})

		b.Run("json", func(b *testing.B) {
			logger := slog.New(NewSlogJSONHandler(io.Discard, slog.LevelDebug)).With(trace.Component, "test")
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				l := logger.With("test", 123).With("animal", "llama\n").With("error", err)
				l.With(trace.ComponentFields, fields).Info("Adding diagnostic debugging handlers. To connect with profiler, use `go tool pprof diag_addr`\n.", "diag_addr", &addr)
			}
		})
	})
}
