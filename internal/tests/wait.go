package tests

import (
	"context"
	"time"
)

// Wait executes condFn, until it is either successful, or ctx is
// cancelled.
func Wait(ctx context.Context, checkPeriod time.Duration, condFn func() error) error {
	for {
		if err := condFn(); err == nil {
			return nil
		}

		timer := time.NewTimer(checkPeriod)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
