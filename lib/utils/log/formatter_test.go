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
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/lib/utils"
)

const message = "Adding diagnostic debugging handlers.\t To connect with profiler, use `go tool pprof diag_addr`."

var (
	logErr = errors.New("the quick brown fox jumped really high")
	addr   = utils.NetAddr{Addr: "127.0.0.1:1234", AddrNetwork: "tcp"}

	fields = logrus.Fields{
		"local":        &addr,
		"remote":       &addr,
		"login":        "llama",
		"teleportUser": "user",
		"id":           1234,
	}
)

func TestOutput(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		fieldsRegex := regexp.MustCompile(`(\w+):((?:"[^"]*"|\[[^]]*]|\S+))\s*`)
		outputRegex := regexp.MustCompile("(\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}-\\d{2}:\\d{2}\\s.*\\[.*]\\s+)(\".*diag_addr`\\.\"\\s)(.*)(log/formatter_test.go:\\d+)")

		tests := []struct {
			name        string
			logrusLevel logrus.Level
			slogLevel   slog.Level
		}{
			{
				name:        "trace",
				logrusLevel: logrus.TraceLevel,
				slogLevel:   slog.LevelDebug - 1,
			},
			{
				name:        "debug",
				logrusLevel: logrus.DebugLevel,
				slogLevel:   slog.LevelDebug,
			},
			{
				name:        "info",
				logrusLevel: logrus.InfoLevel,
				slogLevel:   slog.LevelInfo,
			},
			{
				name:        "warn",
				logrusLevel: logrus.WarnLevel,
				slogLevel:   slog.LevelWarn,
			},
			{
				name:        "error",
				logrusLevel: logrus.ErrorLevel,
				slogLevel:   slog.LevelError,
			},
			{
				name:        "fatal",
				logrusLevel: logrus.FatalLevel,
				slogLevel:   slog.LevelError + 1,
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				// Create a logrus logger using the custom formatter which outputs to a local buffer.
				var logrusOutput bytes.Buffer
				formatter := NewDefaultTextFormatter(true)
				formatter.timestampEnabled = true
				require.NoError(t, formatter.CheckAndSetDefaults())

				logger := logrus.New()
				logger.SetFormatter(formatter)
				logger.SetOutput(&logrusOutput)
				logger.ReplaceHooks(logrus.LevelHooks{})
				logger.SetLevel(test.logrusLevel)
				entry := logger.WithField(trace.Component, "test")

				// Create a slog logger using the custom handler which outputs to a local buffer.
				var slogOutput bytes.Buffer
				slogLogger := slog.New(NewSLogTextHandler(&slogOutput, test.slogLevel, true)).With(trace.Component, "test")

				// Add some fields and output the message at the desired log level via logrus.
				l := entry.WithField("test", 123).WithField("animal", "llama\n").WithField("error", logErr)
				l.WithField("diag_addr", &addr).WithField(trace.ComponentFields, fields).Log(test.logrusLevel, message)

				// Add some fields and output the message at the desired log level via slog.
				l2 := slogLogger.With("test", 123).With("animal", "llama\n").With("error", logErr)
				l2.With(trace.ComponentFields, fields).Log(context.Background(), test.slogLevel, message, "diag_addr", &addr)

				// Validate that both loggers produces the same output. The added complexity comes from the fact that
				// our custom slog handler does NOT sort the additional fields like our logrus formatter does.
				logrusMatches := outputRegex.FindStringSubmatch(logrusOutput.String())
				require.NotEmpty(t, logrusMatches, "logrus output was in unexpected format: %s", logrusOutput.String())
				slogMatches := outputRegex.FindStringSubmatch(slogOutput.String())
				require.NotEmpty(t, slogMatches, "slog output was in unexpected format: %s", slogOutput.String())

				// The first match is the timestamp, level, and component: 2023-10-31T10:09:06-04:00 DEBU [TEST]
				assert.Empty(t, cmp.Diff(logrusMatches[1], slogMatches[1]), "expected timestamp, level, and component to be identical")
				// The second match is the log message: "Adding diagnostic debugging handlers.\t To connect with profiler, use `go tool pprof diag_addr`.\n"
				assert.Empty(t, cmp.Diff(logrusMatches[2], slogMatches[2]), "expected output messages to be identical")
				// The last matches are the caller information
				assert.Equal(t, "log/formatter_test.go:116", logrusMatches[4])
				assert.Equal(t, "log/formatter_test.go:120", slogMatches[4])

				// The third matches are the fields which will be key value pairs(animal:llama) separated by a space. Since
				// logrus sorts the fields and slog doesn't we can't just assert equality and instead build a map of the key
				// value pairs to ensure they are all present and accounted for.
				logrusFieldMatches := fieldsRegex.FindAllStringSubmatch(logrusMatches[3], -1)
				slogFieldMatches := fieldsRegex.FindAllStringSubmatch(slogMatches[3], -1)

				// The first match is the key, the second match is the value
				logrusFields := map[string]string{}
				for _, match := range logrusFieldMatches {
					logrusFields[strings.TrimSpace(match[1])] = strings.TrimSpace(match[2])
				}

				slogFields := map[string]string{}
				for _, match := range slogFieldMatches {
					slogFields[strings.TrimSpace(match[1])] = strings.TrimSpace(match[2])
				}

				assert.Equal(t, slogFields, logrusFields)
			})
		}
	})

	t.Run("json", func(t *testing.T) {
		tests := []struct {
			name        string
			logrusLevel logrus.Level
			slogLevel   slog.Level
		}{
			{
				name:        "trace",
				logrusLevel: logrus.TraceLevel,
				slogLevel:   slog.LevelDebug - 1,
			},
			{
				name:        "debug",
				logrusLevel: logrus.DebugLevel,
				slogLevel:   slog.LevelDebug,
			},
			{
				name:        "info",
				logrusLevel: logrus.InfoLevel,
				slogLevel:   slog.LevelInfo,
			},
			{
				name:        "warn",
				logrusLevel: logrus.WarnLevel,
				slogLevel:   slog.LevelWarn,
			},
			{
				name:        "error",
				logrusLevel: logrus.ErrorLevel,
				slogLevel:   slog.LevelError,
			},
			{
				name:        "fatal",
				logrusLevel: logrus.FatalLevel,
				slogLevel:   slog.LevelError + 1,
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				// Create a logrus logger using the custom formatter which outputs to a local buffer.
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
				logger.SetLevel(test.logrusLevel)
				entry := logger.WithField(trace.Component, "test")

				// Create a slog logger using the custom formatter which outputs to a local buffer.
				var slogOutput bytes.Buffer
				slogLogger := slog.New(NewSlogJSONHandler(&slogOutput, test.slogLevel)).With(trace.Component, "test")

				// Add some fields and output the message at the desired log level via logrus.
				l := entry.WithField("test", 123).WithField("animal", "llama").WithField("error", logErr)
				l.WithField("diag_addr", &addr).Log(test.logrusLevel, message)

				// Add some fields and output the message at the desired log level via slog.
				l2 := slogLogger.With("test", 123).With("animal", "llama").With("error", logErr)
				l2.Log(context.Background(), test.slogLevel, message, "diag_addr", &addr)

				// The order of the fields emitted by the two loggers is different, so comparing the output directly
				// for equality won't work. Instead, a map is built with all the key value pairs, excluding the caller
				// and that map is compared to ensure all items are present and match.
				var logrusData map[string]any
				require.NoError(t, json.Unmarshal(logrusOut.Bytes(), &logrusData), "invalid logrus output format")

				var slogData map[string]any
				require.NoError(t, json.Unmarshal(slogOutput.Bytes(), &slogData), "invalid slog output format")

				logrusCaller, ok := logrusData["caller"].(string)
				delete(logrusData, "caller")
				assert.True(t, ok, "caller was missing from logrus output")
				assert.Equal(t, "log/formatter_test.go:220", logrusCaller)

				slogCaller, ok := slogData["caller"].(string)
				delete(slogData, "caller")
				assert.True(t, ok, "caller was missing from slog output")
				assert.Equal(t, "log/formatter_test.go:224", slogCaller)

				require.Empty(t,
					cmp.Diff(
						logrusData,
						slogData,
						cmpopts.SortMaps(func(a, b string) bool { return a < b }),
					),
				)
			})
		}
	})
}

func BenchmarkFormatter(b *testing.B) {
	b.ReportAllocs()
	b.Run("logrus", func(b *testing.B) {
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
				l := entry.WithField("test", 123).WithField("animal", "llama\n").WithField("error", logErr)
				l.WithField("diag_addr", &addr).WithField(trace.ComponentFields, fields).Info(message)
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
				l := entry.WithField("test", 123).WithField("animal", "llama\n").WithField("error", logErr)
				l.WithField("diag_addr", &addr).WithField(trace.ComponentFields, fields).Info(message)
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
				l := logger.With("test", 123).With("animal", "llama\n").With("error", logErr)
				l.With(trace.ComponentFields, fields).Info(message, "diag_addr", &addr)
			}
		})

		b.Run("text", func(b *testing.B) {
			logger := slog.New(NewSLogTextHandler(io.Discard, slog.LevelDebug, true)).With(trace.Component, "test")
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				l := logger.With("test", 123).With("animal", "llama\n").With("error", logErr)
				l.With(trace.ComponentFields, fields).Info(message, "diag_addr", &addr)
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
				l := logger.With("test", 123).With("animal", "llama\n").With("error", logErr)
				l.With(trace.ComponentFields, fields).Info(message, "diag_addr", &addr)
			}
		})

		b.Run("json", func(b *testing.B) {
			logger := slog.New(NewSlogJSONHandler(io.Discard, slog.LevelDebug)).With(trace.Component, "test")
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				l := logger.With("test", 123).With("animal", "llama\n").With("error", logErr)
				l.With(trace.ComponentFields, fields).Info(message, "diag_addr", &addr)
			}
		})
	})
}
