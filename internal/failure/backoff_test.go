package failure_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/openzot/openzot/internal/failure"
)

// The pause doubles per consecutive retry so a persistent outage is not retried
// at the same rate as a one-off blip, and is capped so a long continuation
// budget cannot leave a run asleep for hours.
func TestBackoffDoublesAndIsCapped(t *testing.T) {
	base := time.Second

	got := failure.BackoffFor(base, 1)
	assert.Equal(t, base, got, "first retry waits %s, want %s", got, base)

	got = failure.BackoffFor(base, 2)
	assert.Equal(t, 2*base, got, "second retry waits %s, want %s", got, 2*base)

	got = failure.BackoffFor(base, 3)
	assert.Equal(t, 4*base, got, "third retry waits %s, want %s", got, 4*base)

	got = failure.BackoffFor(base, 40)
	assert.Equal(t, failure.MaxRetryBackoff, got, "a long outage waits %s, want the cap %s", got, failure.MaxRetryBackoff)

	// the cap binds the base too. A caller-configured backoff above it must not
	// make the first retry the longest wait of the run
	got = failure.BackoffFor(2*failure.MaxRetryBackoff, 1)
	assert.Equal(t, failure.MaxRetryBackoff, got, "a base above the cap waits %s on the first retry, want the cap %s", got, failure.MaxRetryBackoff)
}

// A provider that advises an absurd Retry-After must not park an unattended run
// for hours. The advice is honored up to a cap, and no further.
func TestAnAbsurdRetryAfterIsCapped(t *testing.T) {
	assert.Equal(t, failure.MaxRateLimitWait, failure.RateLimitWait(48*time.Hour, true, time.Second))

	// advice inside the cap is followed exactly, rather than rounded to our own
	// backoff schedule
	assert.Equal(t, 90*time.Second, failure.RateLimitWait(90*time.Second, true, time.Second))

	// and with no advice at all the ordinary backoff applies
	assert.Equal(t, 4*time.Second, failure.RateLimitWait(0, false, 4*time.Second), "want the fallback backoff")
}

// The backoff is a floor under the provider's advice, not only a fallback for its absence. "Retry-After: 0" is advice to
// retry now, and a provider that keeps sending it would otherwise be hammered with the tight loop the backoff prevents.
func TestAZeroRetryAfterIsFlooredByTheBackoff(t *testing.T) {
	assert.Equal(t, 4*time.Second, failure.RateLimitWait(0, true, 4*time.Second), "want the 4s backoff floor under \"retry now\"")

	// advice above the floor still wins. The provider knows its own window
	assert.Equal(t, 90*time.Second, failure.RateLimitWait(90*time.Second, true, 4*time.Second), "want the advised 90s over the smaller backoff")
}
