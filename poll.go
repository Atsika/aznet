package aznet

import "time"

// AdaptivePoll implements an exponential back-off sleep utility.
// Call Reset() after any activity to return to the fast interval.
type AdaptivePoll struct {
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

// Next returns the interval to wait before the next poll and then backs off
// exponentially up to Steady. It returns 0 exactly once after Reset() (without
// advancing the back-off) so activity resumes at full speed. The caller owns the
// actual waiting, which keeps AdaptivePoll single-goroutine and lock-free.
func (p *AdaptivePoll) Next() time.Duration {
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
	p.Cur = p.Fast
	p.skip = true
}
