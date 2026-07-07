// Package feedback provides an in-memory ring buffer of recent warn+ log
// records and an aggregated counter of usage events whose model was missing
// from the price table. Both are surfaced in the dashboard's "Feedback"
// panel via the server's GET /api/feedback endpoint so operational problems
// (a stale prices.yaml, a renamed model, parse failures) are visible to a
// user running the tray app with no console.
//
// The buffer is wired as a slog.Handler that TEES warn-and-above records into
// itself while passing every record through to an inner handler (console or
// rotated log file). Unknown-model events are NOT logged per event — one
// usage event per message would flood the buffer with the exact model that is
// missing — so they are aggregated per model name instead.
package feedback

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// DefaultCapacity is the ring buffer size used by the process-wide default
// buffer. ~200 recent warnings is plenty to diagnose a misconfiguration
// without holding meaningful memory.
const DefaultCapacity = 200

// Record is one buffered log record. Attrs are flattened to strings so the
// value is always JSON-serializable regardless of the original attribute
// types (errors, durations, etc.).
type Record struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Buffer is a concurrency-safe fixed-capacity ring buffer of Records.
type Buffer struct {
	mu   sync.Mutex
	buf  []Record
	next int // index of the next write
	size int // number of valid entries (<= cap)
}

// NewBuffer returns a Buffer holding at most capacity records. A capacity <= 0
// is coerced to 1 so the buffer is always usable.
func NewBuffer(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &Buffer{buf: make([]Record, capacity)}
}

// Add appends a record, evicting the oldest when the buffer is full.
func (b *Buffer) Add(r Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf[b.next] = r
	b.next = (b.next + 1) % len(b.buf)
	if b.size < len(b.buf) {
		b.size++
	}
}

// Recent returns the buffered records newest-first. The returned slice is a
// fresh copy and never nil (empty slice when nothing has been buffered), so
// callers can JSON-encode it directly as [].
func (b *Buffer) Recent() []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Record, 0, b.size)
	// Walk backwards from the most recently written slot.
	for i := 0; i < b.size; i++ {
		idx := (b.next - 1 - i + len(b.buf)*2) % len(b.buf)
		out = append(out, b.buf[idx])
	}
	return out
}

// Len reports how many records are currently buffered.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

// Reset empties the buffer. Intended for test isolation.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.buf {
		b.buf[i] = Record{}
	}
	b.next = 0
	b.size = 0
}

// UnknownModel is the aggregate for a single model name that could not be
// priced.
type UnknownModel struct {
	Model     string    `json:"model"`
	Count     int64     `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// UnknownModels is a concurrency-safe per-model counter of usage events whose
// model was absent from the price table.
type UnknownModels struct {
	mu sync.Mutex
	m  map[string]*UnknownModel
}

// NewUnknownModels returns an empty aggregate.
func NewUnknownModels() *UnknownModels {
	return &UnknownModels{m: make(map[string]*UnknownModel)}
}

// Record notes one unpriced event for model at time t. Empty model names are
// ignored — an absent model is a different, already-handled case and must not
// be aggregated as an "unknown model".
func (u *UnknownModels) Record(model string, t time.Time) {
	if model == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	e, ok := u.m[model]
	if !ok {
		u.m[model] = &UnknownModel{Model: model, Count: 1, FirstSeen: t, LastSeen: t}
		return
	}
	e.Count++
	if t.Before(e.FirstSeen) {
		e.FirstSeen = t
	}
	if t.After(e.LastSeen) {
		e.LastSeen = t
	}
}

// Snapshot returns the aggregates sorted by descending event count (ties
// broken by model name). The returned slice is never nil.
func (u *UnknownModels) Snapshot() []UnknownModel {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]UnknownModel, 0, len(u.m))
	for _, e := range u.m {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// TotalEvents returns the sum of event counts across all unknown models.
func (u *UnknownModels) TotalEvents() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	var total int64
	for _, e := range u.m {
		total += e.Count
	}
	return total
}

// Reset clears the aggregate. Intended for test isolation.
func (u *UnknownModels) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.m = make(map[string]*UnknownModel)
}

// Handler is a slog.Handler that tees warn-and-above records into a Buffer
// while delegating every record to an inner handler. This lets the normal
// logging destination (console or rotated file) stay authoritative while the
// dashboard gains an in-memory view of recent warnings.
type Handler struct {
	inner    slog.Handler
	buf      *Buffer
	minLevel slog.Level
	attrs    map[string]string // accumulated via WithAttrs, flattened to strings
}

// NewHandler wraps inner so warn+ records are also copied into buf.
func NewHandler(inner slog.Handler, buf *Buffer) *Handler {
	return &Handler{inner: inner, buf: buf, minLevel: slog.LevelWarn}
}

// Enabled defers to the inner handler so the tee never changes which records
// reach the underlying destination.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle copies warn+ records into the buffer, then forwards to inner.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if h.buf != nil && r.Level >= h.minLevel {
		rec := Record{Time: r.Time, Level: r.Level.String(), Message: r.Message}
		if r.NumAttrs() > 0 || len(h.attrs) > 0 {
			m := make(map[string]string, r.NumAttrs()+len(h.attrs))
			for k, v := range h.attrs {
				m[k] = v
			}
			r.Attrs(func(a slog.Attr) bool {
				m[a.Key] = a.Value.String()
				return true
			})
			rec.Attrs = m
		}
		h.buf.Add(rec)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a handler that adds attrs to both the inner handler and
// the buffered-record snapshot.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	m := make(map[string]string, len(h.attrs)+len(attrs))
	for k, v := range h.attrs {
		m[k] = v
	}
	for _, a := range attrs {
		m[a.Key] = a.Value.String()
	}
	return &Handler{inner: h.inner.WithAttrs(attrs), buf: h.buf, minLevel: h.minLevel, attrs: m}
}

// WithGroup delegates grouping to the inner handler. Group prefixing is not
// applied to buffered attribute keys — the buffer is a flat diagnostic view.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), buf: h.buf, minLevel: h.minLevel, attrs: h.attrs}
}

// Process-wide defaults. The tray app wires slog to tee into defaultBuffer and
// both ingest paths (tailer + HTTP /log) feed defaultUnknown; the /api/feedback
// handler reads the same instances so everything lines up without explicit
// plumbing through every constructor.
var (
	defaultBuffer  = NewBuffer(DefaultCapacity)
	defaultUnknown = NewUnknownModels()
)

// DefaultBuffer returns the process-wide warning buffer.
func DefaultBuffer() *Buffer { return defaultBuffer }

// DefaultUnknownModels returns the process-wide unknown-model aggregate.
func DefaultUnknownModels() *UnknownModels { return defaultUnknown }

// RecordUnknownModel records one unpriced event against the default aggregate.
func RecordUnknownModel(model string, t time.Time) { defaultUnknown.Record(model, t) }
