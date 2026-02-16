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

// Sleep waits for the current interval and then backs off exponentially up to Steady.
func (p *AdaptivePoll) Sleep() {
	if p.skip {
		p.skip = false
		return
	}
	time.Sleep(p.Cur)
	if p.Cur < p.Steady {
		p.Cur *= 2
		if p.Cur > p.Steady {
			p.Cur = p.Steady
		}
	}
}

// Reset moves the current interval back to the fast value.
func (p *AdaptivePoll) Reset() {
	p.Cur = p.Fast
	p.skip = true
}
