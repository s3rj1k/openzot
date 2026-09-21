package failure

import "time"

// The retry policy. How long a run waits before it tries a failed request again.
const (
	// DefaultRetryBackoff is the pause before the first retry of a retriable
	// provider failure. Each consecutive retry doubles it, up to MaxRetryBackoff.
	//
	// Retrying instantly is worse than not retrying. A provider outage burns the
	// whole continuation budget inside a few milliseconds - so a run dies to a
	// blip it would have outlived - while hammering an endpoint that is already
	// failing. The delay is what turns the continuation budget into a window of
	// time rather than a count of round trips.
	DefaultRetryBackoff = 1 * time.Second

	// MaxRetryBackoff caps the doubling, so a long continuation budget cannot
	// leave a run asleep for hours on an outage that has already ended.
	MaxRetryBackoff = 30 * time.Second

	// MaxRateLimitWait caps how long a run will sit out a provider-advised
	// Retry-After. Sitting out a real rate-limit window is the right thing for an
	// unattended run - it is why a 429 no longer ends one - but no legitimate
	// Window needs longer than this, and a mistaken or hostile header must not be
	// able to park a run for hours.
	MaxRateLimitWait = 5 * time.Minute
)

// BackoffFor is the pause before the attempt'th consecutive retry. It is base, doubling per attempt, capped at
// MaxRetryBackoff even when base alone exceeds the cap, so no generous RetryBackoff makes the first retry the longest wait.
// A non-positive base means no wait at all.
func BackoffFor(base time.Duration, attempt int) time.Duration {
	if base <= 0 || attempt <= 0 {
		return 0
	}

	delay := base

	if delay >= MaxRetryBackoff {
		return MaxRetryBackoff
	}

	for i := 1; i < attempt; i++ {
		delay *= 2

		if delay >= MaxRetryBackoff {
			return MaxRetryBackoff
		}
	}

	return delay
}

// RateLimitWait is how long to sit out a rate limit, the larger of the provider's advised delay and the ordinary backoff. The
// backoff is a floor, so a "Retry-After: 0" cannot become a tight loop. The advice is capped, since an unattended run must not
// be parked for hours by a mistaken or hostile header.
func RateLimitWait(advised time.Duration, ok bool, fallback time.Duration) time.Duration {
	if !ok {
		return fallback
	}

	if advised > MaxRateLimitWait {
		advised = MaxRateLimitWait
	}

	if advised < fallback {
		return fallback
	}

	return advised
}
