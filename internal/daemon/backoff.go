package daemon

import (
	"math/rand/v2"
	"time"
)

type backoff struct {
	min, max, cur time.Duration
}

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else {
		b.cur *= 2
		if b.cur > b.max {
			b.cur = b.max
		}
	}
	return jitter(b.cur)
}

func (b *backoff) reset() { b.cur = 0 }

// jitter applies ±20% randomness so a fleet does not reconnect in lockstep.
func jitter(d time.Duration) time.Duration {
	factor := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(d) * factor)
}

// supersededError means another process registered the same device ID. Retrying
// immediately would start a reconnect war, so Run backs off for minutes.
type supersededError struct{}

func (*supersededError) Error() string {
	return "another connection superseded this device"
}
