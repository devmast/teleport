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
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravitational/trace"
)

type SlogTextHandler struct {
	level          slog.Leveler
	enableColors   bool
	component      string
	preformatted   []byte   // data from WithGroup and WithAttrs
	unopenedGroups []string // groups from WithGroup that haven't been opened
	mu             *sync.Mutex
	out            io.Writer
}

func NewSLogTextHandler(w io.Writer, level slog.Leveler, enableColors bool) *SlogTextHandler {
	return &SlogTextHandler{
		level:        level,
		enableColors: enableColors,
		out:          w,
		mu:           &sync.Mutex{},
	}
}

func (s *SlogTextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= s.level.Level()
}

func (s *SlogTextHandler) appendAttr(buf []byte, a slog.Attr) []byte {
	// Resolve the Attr's value before doing anything else.
	a.Value = a.Value.Resolve()
	// Ignore empty Attrs.
	if a.Equal(slog.Attr{}) {
		return buf
	}

	switch a.Value.Kind() {
	case slog.KindString:
		value := a.Value.String()
		if needsQuoting(value) {
			if a.Key == trace.Component || a.Key == slog.LevelKey || a.Key == "caller" || a.Key == slog.MessageKey {
				buf = fmt.Append(buf, " ")
			} else {
				buf = fmt.Appendf(buf, " %s:", a.Key)
			}
			buf = strconv.AppendQuote(buf, value)
			break
		}

		if a.Key == trace.Component || a.Key == slog.LevelKey || a.Key == "caller" || a.Key == slog.MessageKey {
			buf = fmt.Appendf(buf, " %s", a.Value.String())
			break
		}

		buf = fmt.Appendf(buf, " %s:%s", a.Key, a.Value.String())
	case slog.KindGroup:
		attrs := a.Value.Group()
		// Ignore empty groups.
		if len(attrs) == 0 {
			return buf
		}
		// If the key is non-empty, write it out and indent the rest of the attrs.
		// Otherwise, inline the attrs.
		if a.Key != "" {
			buf = fmt.Appendf(buf, " %s:", a.Key)
		}
		for _, ga := range attrs {
			buf = s.appendAttr(buf, ga)
		}
	default:
		switch err := a.Value.Any().(type) {
		case trace.Error:
			buf = fmt.Appendf(buf, " error:[%v]", err.DebugReport())
		case error:
			buf = fmt.Appendf(buf, " error:[%v]", a.Value)
		default:
			buf = fmt.Appendf(buf, " %s:%s", a.Key, a.Value)
		}
	}
	return buf
}

// writeTimeRFC3339 writes the time in [time.RFC3339Nano] to the buffer.
// This takes half the time of [time.Time.AppendFormat]. Adapted from
// go/src/log/slog/handler.go
func writeTimeRFC3339(buf *buffer, t time.Time) {
	year, month, day := t.Date()
	buf.WritePosIntWidth(year, 4)
	buf.WriteByte('-')
	buf.WritePosIntWidth(int(month), 2)
	buf.WriteByte('-')
	buf.WritePosIntWidth(day, 2)
	buf.WriteByte('T')
	hour, min, sec := t.Clock()
	buf.WritePosIntWidth(hour, 2)
	buf.WriteByte(':')
	buf.WritePosIntWidth(min, 2)
	buf.WriteByte(':')
	buf.WritePosIntWidth(sec, 2)
	_, offsetSeconds := t.Zone()
	if offsetSeconds == 0 {
		buf.WriteByte('Z')
	} else {
		offsetMinutes := offsetSeconds / 60
		if offsetMinutes < 0 {
			buf.WriteByte('-')
			offsetMinutes = -offsetMinutes
		} else {
			buf.WriteByte('+')
		}
		buf.WritePosIntWidth(offsetMinutes/60, 2)
		buf.WriteByte(':')
		buf.WritePosIntWidth(offsetMinutes%60, 2)
	}
}

func (s *SlogTextHandler) Handle(ctx context.Context, r slog.Record) error {
	buf := newBuffer()
	defer buf.Free()

	if !r.Time.IsZero() {
		writeTimeRFC3339(buf, r.Time)
	}

	var color int
	var level string
	switch r.Level {
	case slog.LevelDebug - 1:
		level = "TRACE"
		color = gray
	case slog.LevelDebug:
		level = "DEBUG"
		color = gray
	case slog.LevelWarn:
		level = "WARN"
		color = yellow
	case slog.LevelError:
		level = "ERROR"
		color = red
	case slog.LevelError + 1:
		level = "FATAL"
		color = red
	default:
		color = blue
		level = r.Level.String()
	}

	if !s.enableColors {
		color = noColor
	}

	level = padMax(level, trace.DefaultLevelPadding)
	if color == noColor {
		*buf = s.appendAttr(*buf, slog.String(slog.LevelKey, level))
	} else {
		*buf = fmt.Appendf(*buf, " \u001B[%dm%s\u001B[0m", color, level)
	}

	*buf = s.appendAttr(*buf, slog.String(trace.Component, s.component))

	*buf = s.appendAttr(*buf, slog.String(slog.MessageKey, r.Message))

	// Insert preformatted attributes just after built-in ones.
	*buf = append(*buf, s.preformatted...)
	if r.NumAttrs() > 0 {
		*buf = s.appendUnopenedGroups(*buf)
		r.Attrs(func(a slog.Attr) bool {
			*buf = s.appendAttr(*buf, a)
			return true
		})
	}

	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()

		src := &slog.Source{
			Function: f.Function,
			File:     f.File,
			Line:     f.Line,
		}

		file, line := getCaller(slog.Attr{Key: slog.SourceKey, Value: slog.AnyValue(src)})
		*buf = fmt.Appendf(*buf, " %s:%d", file, line)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.out.Write(*buf)
	return err
}

// groupOrAttrs holds either a group name or a list of slog.Attrs.
type groupOrAttrs struct {
	group string      // group name if non-empty
	attrs []slog.Attr // attrs if non-empty
}

//
//func (s *SlogTextHandler) withGroupOrAttrs(goa groupOrAttrs) *SlogTextHandler {
//	s2 := *s
//	s2.goas = make([]groupOrAttrs, len(s.goas)+1)
//	copy(s2.goas, s.goas)
//
//	idx := slices.IndexFunc(goa.attrs, func(attr slog.Attr) bool {
//		return attr.Key == trace.Component
//	})
//
//	component := s.component
//	if idx >= 0 {
//		const padding = trace.DefaultComponentPadding
//		component = fmt.Sprintf("[%v]", goa.attrs[idx].Value.String())
//		component = strings.ToUpper(padMax(component, padding))
//		if component[len(component)-1] != ' ' {
//			component = component[:len(component)-1] + "]"
//		}
//
//		goa.attrs = goa.attrs[:idx+copy(goa.attrs[idx:], goa.attrs[idx+1:])]
//	}
//
//	s2.goas[len(s2.goas)-1] = goa
//	s2.component = component
//	return &s2
//}

func (s *SlogTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return s
	}
	s2 := *s
	// Force an append to copy the underlying array.
	pre := slices.Clip(s.preformatted)
	// Add all groups from WithGroup that haven't already been added.
	s2.preformatted = s2.appendUnopenedGroups(pre)
	// Now all groups have been opened.
	s2.unopenedGroups = nil

	component := s.component

	// Pre-format the attributes.
	for _, a := range attrs {
		if a.Key == trace.Component {
			const padding = trace.DefaultComponentPadding
			component = fmt.Sprintf("[%v]", a.Value.String())
			component = strings.ToUpper(padMax(component, padding))
			if component[len(component)-1] != ' ' {
				component = component[:len(component)-1] + "]"
			}
			continue
		}

		s2.preformatted = s2.appendAttr(s2.preformatted, a)
	}
	s2.component = component
	return &s2
}

func (s *SlogTextHandler) appendUnopenedGroups(buf []byte) []byte {
	for _, g := range s.unopenedGroups {
		buf = fmt.Appendf(buf, "%s:", g)
	}
	return buf
}

func (s *SlogTextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return s
	}

	s2 := *s
	// Add an unopened group to h2 without modifying h.
	s2.unopenedGroups = make([]string, len(s.unopenedGroups)+1)
	copy(s2.unopenedGroups, s.unopenedGroups)
	s2.unopenedGroups[len(s2.unopenedGroups)-1] = name
	return &s2
}

type SlogJSONHandler struct {
	handler *slog.JSONHandler
}

func NewSlogJSONHandler(w io.Writer, level slog.Leveler) *SlogJSONHandler {
	return &SlogJSONHandler{
		handler: slog.NewJSONHandler(w, &slog.HandlerOptions{
			AddSource: true,
			Level:     level,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				switch a.Key {
				case trace.Component:
					a.Key = "component"
				case slog.LevelKey:
					a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
				case slog.TimeKey:
					a.Key = "timestamp"
					a.Value = slog.StringValue(a.Value.Time().Format(time.RFC3339))
				case slog.MessageKey:
					a.Key = "message"
				case slog.SourceKey:
					file, line := getCaller(a)
					a = slog.String("caller", fmt.Sprintf("%s:%d", file, line))
				}

				return a
			},
		}),
	}
}

func getCaller(a slog.Attr) (file string, line int) {
	s := a.Value.Any().(*slog.Source)
	count := 0
	idx := strings.LastIndexFunc(s.File, func(r rune) bool {
		if r == '/' {
			count++
		}

		return count == 2
	})
	file = s.File[idx+1:]
	line = s.Line

	return
}

func (s *SlogJSONHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return s.handler.Enabled(ctx, level)
}

func (s *SlogJSONHandler) Handle(ctx context.Context, record slog.Record) error {
	return s.handler.Handle(ctx, record)
}

func (s *SlogJSONHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return s.handler.WithAttrs(attrs)
}

func (s *SlogJSONHandler) WithGroup(name string) slog.Handler {
	return s.handler.WithGroup(name)
}
