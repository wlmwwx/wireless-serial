package transport

import (
	"context"
	"testing"
	"time"
)

func TestBackoffSequence(t *testing.T) {
	b := NewBackoff(time.Second, 10*time.Second)
	want := []time.Duration{1, 2, 4, 8, 10, 10, 10}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("step %d: Next() = %s, want %s", i, got, w*time.Second)
		}
	}
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Fatalf("after Reset: %s, want 1s", got)
	}
}

func TestBackoffDefaults(t *testing.T) {
	b := NewBackoff(0, 0)
	if b.Min != time.Second || b.Max != time.Second {
		t.Fatalf("defaults = %s/%s", b.Min, b.Max)
	}
	b2 := NewBackoff(5*time.Second, time.Second)
	if b2.Max != 5*time.Second {
		t.Fatalf("Max < Min not corrected: %s", b2.Max)
	}
}

func TestWaitCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx, time.Hour); err == nil {
		t.Fatal("Wait returned nil on a cancelled context")
	}
	start := time.Now()
	if err := Wait(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Fatalf("Wait returned too early: %s", elapsed)
	}
}

func TestDialRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Dial(ctx, "127.0.0.1", 1, 300*time.Millisecond); err == nil {
		t.Fatal("Dial succeeded against a closed port")
	}
}
