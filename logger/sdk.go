// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package logger

import (
	"context"
	"log/slog"
	"maps"
)

// componentKey is the attribute the SDK packages (spawnllm, ctxengine,
// cogmem, ...) name themselves with on every record.
const componentKey = "component"

// sdkLogger is the one logger every SDK object the host builds is given.
var sdkLogger = slog.New(sdkHandler{})

// SDK returns the logger to pass to every SDK package: its records go through
// this package, to the same console, agency.log and error.log, at the same
// levels and with the same component names as the host's own lines.
func SDK() *slog.Logger { return sdkLogger }

// SDKAs is SDK for an SDK object whose lines the host has always logged under
// a component of its own: records the SDK tags with component from are logged
// as component to; any other component is kept.
func SDKAs(from, to string) *slog.Logger {
	return slog.New(sdkHandler{rename: map[string]string{from: to}})
}

// sdkHandler is a slog.Handler over this package's logger. A record's
// top-level "component" attribute becomes the line's component; every other
// attribute becomes a field. Attributes inside a group are named
// "group.key", so nothing is lost or merged.
type sdkHandler struct {
	component string            // set by WithAttrs with a top-level component
	fields    map[string]any    // attributes added by WithAttrs, already prefixed
	prefix    string            // the open groups, "a.b." for WithGroup("a").WithGroup("b")
	rename    map[string]string // component as the SDK names it -> as the host logs it
}

// hostLevel maps a slog level onto this package's levels, as the SDK's former
// logging backends did: below info is debug, then info, warn and error.
func hostLevel(l slog.Level) LogLevel {
	switch {
	case l < slog.LevelInfo:
		return DEBUG
	case l < slog.LevelWarn:
		return INFO
	case l < slog.LevelError:
		return WARN
	default:
		return ERROR
	}
}

func (h sdkHandler) Enabled(_ context.Context, l slog.Level) bool {
	mu.RLock()
	minLevel := currentLevel
	mu.RUnlock()
	return hostLevel(l) >= minLevel
}

func (h sdkHandler) Handle(_ context.Context, r slog.Record) error {
	component := h.component
	var fields map[string]any
	if len(h.fields) > 0 || r.NumAttrs() > 0 {
		fields = make(map[string]any, len(h.fields)+r.NumAttrs())
		maps.Copy(fields, h.fields)
	}
	r.Attrs(func(a slog.Attr) bool {
		addAttr(fields, &component, h.prefix, a)
		return true
	})
	if len(fields) == 0 {
		fields = nil
	}
	if to, ok := h.rename[component]; ok {
		component = to
	}
	// The caller is the record's own call site, not this handler.
	logMessageAt(hostLevel(r.Level), component, r.Message, fields, r.PC)
	return nil
}

func (h sdkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	out := sdkHandler{component: h.component, prefix: h.prefix, rename: h.rename, fields: make(map[string]any, len(h.fields)+len(attrs))}
	maps.Copy(out.fields, h.fields)
	for _, a := range attrs {
		addAttr(out.fields, &out.component, h.prefix, a)
	}
	return out
}

func (h sdkHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	out := h
	out.prefix = h.prefix + name + "."
	return out
}

// addAttr adds a to fields under prefix, following slog's handler rules: the
// value is resolved, an empty attribute is ignored, a group is flattened
// (inline when it has no key, skipped when it has no attributes). An
// ungrouped string "component" sets *component instead of becoming a field.
func addAttr(fields map[string]any, component *string, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		if len(group) == 0 {
			return
		}
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, g := range group {
			addAttr(fields, component, prefix, g)
		}
		return
	}
	if prefix == "" && a.Key == componentKey && a.Value.Kind() == slog.KindString {
		*component = a.Value.String()
		return
	}
	fields[prefix+a.Key] = a.Value.Any()
}
