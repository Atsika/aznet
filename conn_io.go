package aznet

import (
	"context"
	"os"
	"sync"
	"time"
)

// ioDeadline cancels all current operations when its mutable deadline expires.
// Moving or clearing a deadline before expiry leaves those operations alive.
type ioDeadline struct {
	active     map[context.Context]context.CancelCauseFunc
	timer      *time.Timer
	when       time.Time
	mu         sync.Mutex
	generation uint64
}

func (d *ioDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.when = t
	d.generation++
	generation := d.generation
	if d.timer != nil {
		d.timer.Stop()
	}
	if t.IsZero() {
		return
	}
	expire := func() {
		for _, cancel := range d.active {
			cancel(os.ErrDeadlineExceeded)
		}
	}
	if !time.Now().Before(t) {
		expire()
		return
	}
	d.timer = time.AfterFunc(time.Until(t), func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.generation == generation {
			expire()
		}
	})
}

func (d *ioDeadline) operation(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	d.mu.Lock()
	if d.active == nil {
		d.active = make(map[context.Context]context.CancelCauseFunc)
	}
	d.active[ctx] = cancel
	if !d.when.IsZero() && !time.Now().Before(d.when) {
		cancel(os.ErrDeadlineExceeded)
	}
	d.mu.Unlock()
	return ctx, func() {
		d.mu.Lock()
		delete(d.active, ctx)
		d.mu.Unlock()
		cancel(context.Canceled)
	}
}

// lockIO makes waiting for another operation interruptible as well as the I/O.
func lockIO(ctx context.Context, gate chan struct{}) error {
	select {
	case gate <- struct{}{}:
		if err := context.Cause(ctx); err != nil {
			<-gate
			return err
		}
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
