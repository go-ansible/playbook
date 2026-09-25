package playbook

import "sync"

// forkLimiter caps how many hosts do work at once — Engine.Forks — and
// hands the next permit to whoever has been waiting LONGEST rather
// than to whichever goroutine happens to win a race.
//
// The fairness is the point, not a refinement. A buffered channel used
// as a semaphore makes no ordering promise, and Go's runtime does not
// add one: a host running fast tasks releases and re-acquires in a
// tight loop, and can take the permit again before a host that has
// been blocked since the start ever sees it. Under strategy: free,
// where each host advances through the task list on its own, that
// starves the slow host completely — measured with forks=1 and two
// hosts, one sleeping two seconds: real Ansible alternates between
// them task by task, while this port ran the fast host's ENTIRE list
// first and the slow host's afterwards, repeating every task banner.
//
// Real has no such race to lose: its free strategy is one loop that
// offers each host its next task in turn and queues the work, so the
// order comes from the loop rather than from whoever asks first. A
// FIFO queue reproduces that from the other side — a host that
// releases and immediately asks again goes to the BACK, behind the
// host already waiting.
type forkLimiter struct {
	mu      sync.Mutex
	free    int
	waiters []chan struct{}
}

func newForkLimiter(n int) *forkLimiter {
	if n <= 0 {
		return nil // unlimited: every acquire is a no-op
	}
	return &forkLimiter{free: n}
}

// acquire blocks until a permit is free, then returns the matching
// release func. A nil limiter is unlimited, so both are no-ops.
func (l *forkLimiter) acquire() func() {
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	if l.free > 0 {
		l.free--
		l.mu.Unlock()
		return l.release
	}
	ch := make(chan struct{})
	l.waiters = append(l.waiters, ch)
	l.mu.Unlock()
	<-ch // the releasing goroutine hands its permit straight over
	return l.release
}

func (l *forkLimiter) release() {
	l.mu.Lock()
	// Hand the permit directly to the longest-waiting host rather than
	// returning it to the pool: returning it would let a caller that
	// re-acquires immediately take it back before the waiter runs,
	// which is the starvation this type exists to prevent.
	if len(l.waiters) > 0 {
		ch := l.waiters[0]
		l.waiters = l.waiters[1:]
		l.mu.Unlock()
		close(ch)
		return
	}
	l.free++
	l.mu.Unlock()
}
