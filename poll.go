package aznet

import (
	"sync"
	"time"
)

// AdaptivePoll implements an exponential back-off sleep utility.
// Call Reset() after any activity to return to the fast interval.
// Safe for concurrent use.
type AdaptivePoll struct {
	mu     sync.Mutex
	Cur    time.Duration
	Fast   time.Duration
	Steady time.Duration
	skip   bool
}

// NewAdaptivePoll builds a poller initialized to the fast interval.
func NewAdaptivePoll(fast, steady time.Duration) *AdaptivePoll {
	if fast <= 0 {
		fast = DefaultFastPoll
	}
	if steady < fast {
		steady = fast
	}
	return &AdaptivePoll{Cur: fast, Fast: fast, Steady: steady, skip: false}
}

// Next returns the interval to wait before the next poll, backing off
// exponentially up to Steady. It returns 0 once after Reset so activity resumes
// at full speed. The caller does the waiting.
func (p *AdaptivePoll) Next() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.skip {
		p.skip = false
		return 0
	}
	d := p.Cur
	if p.Cur < p.Steady {
		p.Cur *= 2
		if p.Cur > p.Steady {
			p.Cur = p.Steady
		}
	}
	return d
}

// Reset moves the current interval back to the fast value.
func (p *AdaptivePoll) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.Cur = p.Fast
	p.skip = true
}
