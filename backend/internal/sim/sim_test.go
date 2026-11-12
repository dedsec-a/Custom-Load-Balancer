package sim

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolLimitsConcurrency(t *testing.T) {
	p := NewPool(3)
	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			p.Acquire()
			n := cur.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			cur.Add(-1)
			p.Release()
		})
	}
	wg.Wait()
	if peak.Load() != 3 {
		t.Fatalf("peak concurrency = %d, want 3", peak.Load())
	}
	if s := p.Stats(); s.Busy != 0 || s.Waiting != 0 || s.Served != 20 {
		t.Fatalf("unexpected final stats %+v", s)
	}
}

func TestPoolGrowWakesWaiters(t *testing.T) {
	p := NewPool(1)
	p.Acquire()
	done := make(chan struct{})
	go func() { p.Acquire(); close(done) }()
	waitFor(t, func() bool { return p.Stats().Waiting == 1 })

	p.Resize(2) // should let the waiter in without any Release
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiter not woken after growing pool")
	}
}

func TestPoolShrinkTakesEffectOnRelease(t *testing.T) {
	p := NewPool(2)
	p.Acquire()
	p.Acquire()
	p.Resize(1)
	got := make(chan struct{})
	go func() { p.Acquire(); close(got) }()
	waitFor(t, func() bool { return p.Stats().Waiting == 1 })

	p.Release() // used 2 -> 1, still == size, waiter must keep waiting
	select {
	case <-got:
		t.Fatal("waiter admitted although pool is full after shrink")
	case <-time.After(30 * time.Millisecond):
	}
	p.Release() // used 1 -> 0, now there is room
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("waiter not admitted")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}
