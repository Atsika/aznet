package aznet

import (
	"errors"
	"testing"
	"time"
)

func TestInvalidCredentialDurations(t *testing.T) {
	for _, duration := range []time.Duration{-time.Hour, 0, time.Nanosecond, 999 * time.Millisecond} {
		cfg := applyConfig([]Option{WithSessionDuration(duration)})
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("duration%v Validate=%v", duration, err)
		}
		cfg.cancel()
		l := &Listener{}
		if _, err := l.ConnectionStringFor(duration); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("bootstrap duration%v error=%v", duration, err)
		}
	}
}
