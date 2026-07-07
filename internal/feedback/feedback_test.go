package feedback

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestBufferOrderingNewestFirst(t *testing.T) {
	b := NewBuffer(10)
	b.Add(Record{Message: "first"})
	b.Add(Record{Message: "second"})
	b.Add(Record{Message: "third"})

	got := b.Recent()
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d", len(got))
	}
	want := []string{"third", "second", "first"}
	for i, w := range want {
		if got[i].Message != w {
			t.Errorf("record[%d]: got %q, want %q", i, got[i].Message, w)
		}
	}
}

func TestBufferCapacityEviction(t *testing.T) {
	b := NewBuffer(3)
	for i, m := range []string{"a", "b", "c", "d", "e"} {
		_ = i
		b.Add(Record{Message: m})
	}
	if b.Len() != 3 {
		t.Fatalf("expected len 3 after overflow, got %d", b.Len())
	}
	got := b.Recent()
	want := []string{"e", "d", "c"} // oldest (a, b) evicted
	if len(got) != len(want) {
		t.Fatalf("expected %d records, got %d", len(want), len(got))
	}
	for i, w := range want {
		if got[i].Message != w {
			t.Errorf("record[%d]: got %q, want %q", i, got[i].Message, w)
		}
	}
}

func TestBufferEmptyRecentIsNonNil(t *testing.T) {
	b := NewBuffer(5)
	got := b.Recent()
	if got == nil {
		t.Fatal("Recent() must return a non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d", len(got))
	}
}

func TestBufferReset(t *testing.T) {
	b := NewBuffer(5)
	b.Add(Record{Message: "x"})
	b.Reset()
	if b.Len() != 0 {
		t.Fatalf("expected 0 after reset, got %d", b.Len())
	}
}

func TestHandlerTeesWarnAndAbove(t *testing.T) {
	var out bytes.Buffer
	buf := NewBuffer(50)
	inner := slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewHandler(inner, buf))

	logger.Debug("debug msg")
	logger.Info("info msg")
	logger.Warn("warn msg", "key", "val")
	logger.Error("error msg", "err", "boom")

	got := buf.Recent()
	// Only warn + error should be buffered.
	if len(got) != 2 {
		t.Fatalf("expected 2 buffered records, got %d: %+v", len(got), got)
	}
	if got[0].Message != "error msg" || got[1].Message != "warn msg" {
		t.Errorf("unexpected buffered order: %+v", got)
	}
	if got[1].Attrs["key"] != "val" {
		t.Errorf("expected attr key=val, got %+v", got[1].Attrs)
	}
	if got[0].Level != "ERROR" {
		t.Errorf("expected level ERROR, got %q", got[0].Level)
	}

	// Inner handler must still receive everything (all four lines).
	body := out.String()
	for _, s := range []string{"debug msg", "info msg", "warn msg", "error msg"} {
		if !bytes.Contains([]byte(body), []byte(s)) {
			t.Errorf("inner handler missing %q; body=%s", s, body)
		}
	}
}

func TestHandlerWithAttrs(t *testing.T) {
	var out bytes.Buffer
	buf := NewBuffer(10)
	inner := slog.NewTextHandler(&out, nil)
	logger := slog.New(NewHandler(inner, buf)).With("component", "tailer")

	logger.Warn("something happened", "path", "/tmp/x")

	got := buf.Recent()
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	if got[0].Attrs["component"] != "tailer" {
		t.Errorf("expected component=tailer from WithAttrs, got %+v", got[0].Attrs)
	}
	if got[0].Attrs["path"] != "/tmp/x" {
		t.Errorf("expected path attr, got %+v", got[0].Attrs)
	}
}

func TestHandlerEnabledDefersToInner(t *testing.T) {
	buf := NewBuffer(5)
	inner := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})
	h := NewHandler(inner, buf)
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("expected Info to be disabled when inner min level is Error")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("expected Error to be enabled")
	}
}

func TestUnknownModelsAggregation(t *testing.T) {
	u := NewUnknownModels()
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(2 * time.Minute)

	u.Record("model-x", t1)
	u.Record("model-x", t0) // earlier — should move first_seen back
	u.Record("model-x", t2) // later — should move last_seen forward
	u.Record("model-y", t1)

	snap := u.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 models, got %d", len(snap))
	}
	// Sorted by count desc: model-x (3) before model-y (1).
	if snap[0].Model != "model-x" || snap[0].Count != 3 {
		t.Errorf("expected model-x count 3 first, got %+v", snap[0])
	}
	if !snap[0].FirstSeen.Equal(t0) {
		t.Errorf("first_seen: got %v, want %v", snap[0].FirstSeen, t0)
	}
	if !snap[0].LastSeen.Equal(t2) {
		t.Errorf("last_seen: got %v, want %v", snap[0].LastSeen, t2)
	}
	if snap[1].Model != "model-y" || snap[1].Count != 1 {
		t.Errorf("expected model-y count 1, got %+v", snap[1])
	}
	if u.TotalEvents() != 4 {
		t.Errorf("expected total 4 events, got %d", u.TotalEvents())
	}
}

func TestUnknownModelsIgnoresEmptyModel(t *testing.T) {
	u := NewUnknownModels()
	u.Record("", time.Now())
	if len(u.Snapshot()) != 0 {
		t.Error("empty model name must not be aggregated")
	}
	if u.TotalEvents() != 0 {
		t.Error("empty model must not contribute to total")
	}
}

func TestUnknownModelsSnapshotNonNil(t *testing.T) {
	u := NewUnknownModels()
	if u.Snapshot() == nil {
		t.Error("Snapshot() must return a non-nil slice")
	}
}

// TestConcurrentAccess exercises the buffer and aggregate under concurrent
// writers so `go test -race` can flag any data race.
func TestConcurrentAccess(t *testing.T) {
	b := NewBuffer(64)
	u := NewUnknownModels()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Add(Record{Message: "m"})
				u.Record("model", time.Now())
				_ = b.Recent()
				_ = u.Snapshot()
			}
		}(i)
	}
	wg.Wait()
	if u.TotalEvents() != 2000 {
		t.Errorf("expected 2000 events, got %d", u.TotalEvents())
	}
}
