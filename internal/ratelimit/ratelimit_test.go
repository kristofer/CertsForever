package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestBurstThenRefill(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := New(3, 1, time.Minute) // 3 at once, then 1 per minute
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.Allow("ip") {
			t.Fatalf("burst event %d refused", i)
		}
	}
	if l.Allow("ip") {
		t.Fatal("4th event allowed")
	}
	if !l.Allow("other-ip") {
		t.Fatal("keys aren't independent")
	}
	now = now.Add(59 * time.Second)
	if l.Allow("ip") {
		t.Fatal("refilled too early")
	}
	now = now.Add(2 * time.Second)
	if !l.Allow("ip") {
		t.Fatal("not refilled after a minute")
	}
	if l.Allow("ip") {
		t.Fatal("refilled more than one token")
	}
}

func TestSweepBoundsMemory(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := New(2, 1, time.Second)
	l.now = func() time.Time { return now }
	for i := 0; i < 999; i++ {
		l.Allow(fmt.Sprint("k", i))
	}
	now = now.Add(time.Hour)
	l.Allow("trigger") // 1000th call sweeps
	if n := l.Len(); n > 1 {
		t.Fatalf("%d buckets kept after sweep", n)
	}
}
