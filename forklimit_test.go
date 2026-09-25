package playbook

import (
	"sync"
	"testing"
	"time"
)

// A nil limiter is unlimited and must not block or panic.
func TestForkLimiterNilIsUnlimited(t *testing.T) {
	var l *forkLimiter
	release := l.acquire()
	release()
	if newForkLimiter(0) != nil || newForkLimiter(-1) != nil {
		t.Fatal("Forks <= 0 must give a nil (unlimited) limiter")
	}
}

func TestForkLimiterCapsConcurrency(t *testing.T) {
	l := newForkLimiter(2)
	var mu sync.Mutex
	cur, max := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := l.acquire()
			mu.Lock()
			cur++
			if cur > max {
				max = cur
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			cur--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()
	if max > 2 {
		t.Fatalf("saw %d holders at once, want at most 2", max)
	}
}

// The property a plain buffered channel does NOT have: a caller that
// releases and immediately asks again goes to the BACK of the queue,
// behind whoever was already waiting. Without it, a host running fast
// tasks starves a host that has been blocked since the start.
func TestForkLimiterIsFIFO(t *testing.T) {
	l := newForkLimiter(1)
	release := l.acquire() // the permit is taken

	// Three goroutines queue up, in a known order: each is started
	// only once the previous one is provably waiting.
	var order []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		queued := make(chan struct{})
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			close(queued)
			r := l.acquire()
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			r()
		}(i)
		<-queued
		// Give the goroutine time to actually enter the queue; the
		// close above only says it is about to.
		time.Sleep(20 * time.Millisecond)
	}

	release() // hand it over; each waiter passes it on in turn
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for i, got := range order {
		if got != i+1 {
			t.Fatalf("served in order %v, want 1,2,3 — the queue is not FIFO", order)
		}
	}
}
