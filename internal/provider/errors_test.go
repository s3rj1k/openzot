package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rejected is an error as fantasy reports one. A status and the provider's words.
func refused(status int, message string) error {
	return &fantasy.ProviderError{StatusCode: status, Message: message}
}

func TestIsRetriableUsesStatusOverProse(t *testing.T) {
	saysRetry := &fantasy.ProviderError{StatusCode: 400, Message: "try again", ResponseHeaders: map[string]string{"X-Should-Retry": "true"}}
	saysDont := &fantasy.ProviderError{StatusCode: 400, Message: "no", ResponseHeaders: map[string]string{"x-should-retry": "false"}}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"a 500 retries", refused(500, "boom"), true},
		{"a 503 retries", refused(503, "unavailable"), true},
		{"a 408 retries", refused(408, "timed out"), true},
		{"a 409 retries, as fantasy has it", refused(409, "conflict"), true},
		{"a provider saying so retries even a 400", saysRetry, true},
		{"a provider saying not to does not", saysDont, false},
		{"a 400 does not, whatever it says", refused(400, "internal server error"), false},
		{"a 404 does not, even naming a gateway fault", refused(404, "bad gateway upstream"), false},
		{"a 401 does not", refused(401, "bad key"), false},
		{"a 429 is handled by backoff, not retried", refused(429, "slow down"), false},
		{"nil is not a failure", nil, false},
	}

	for _, test := range tests {
		assert.Equal(t, test.want, IsRetriable(test.err))
	}
}

// With no status to go by, what the error is decides. A stream that ended
// mid-turn, a reset connection and zot's own stall are transient however they
// are worded around.
func TestIsRetriableRecognisesTransportFailuresByType(t *testing.T) {
	reset := &url.Error{Op: litPost, URL: litHTTPGw, Err: &net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}

	for name, err := range map[string]error{
		"a stream cut short":  &fantasy.ProviderError{Message: "stream transport error", Cause: io.ErrUnexpectedEOF},
		"a bare EOF":          fmt.Errorf("read: %w", io.EOF),
		"a connection reset":  reset,
		"a stall":             fmt.Errorf("%w: nothing arrived for 10m", errStreamStalled),
		"a wrapped stall":     fmt.Errorf("run: %w", fmt.Errorf("%w: nothing arrived", errStreamStalled)),
		"a broken pipe":       &url.Error{Op: litPost, URL: litHTTPGw, Err: &net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)}},
		"a closed connection": &url.Error{Op: litPost, URL: litHTTPGw, Err: &net.OpError{Op: "write", Err: net.ErrClosed}},
	} {
		assert.True(t, IsRetriable(err), "%s should be retriable", name)
	}
}

// The other half of going by type. Words alone decide nothing. These read like
// transient faults and are not, unless something typed says so.
func TestIsRetriableIgnoresWhatAnErrorMerelySays(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}

	for name, err := range map[string]error{
		"an invalid key":        errors.New("invalid api key"),
		"a missing model":       errors.New("model not found"),
		"a bad parameter":       errors.New("timeout must be a positive integer"),
		"a gateway's wording":   errors.New("Bad Gateway"),
		"an overload's wording": errors.New("the model is overloaded"),
		"a reset's wording":     errors.New("connection reset by peer"),
		"a refused connection":  refused,
		"a cancellation":        context.Canceled,
	} {
		assert.False(t, IsRetriable(err), "%s should not be retriable", name)
	}
}

// fantasy flags a failure delivered inside an open stream as transient. It has
// no status, and it is the shape of "the generation failed, ask again".
func TestATransientErrorWithNoStatusIsRetriable(t *testing.T) {
	err := &fantasy.ProviderError{Message: "the upstream fell over", TransientError: true}

	assert.True(t, IsRetriable(err), "a transient failure with no status should retry")
}

func TestRateLimitIsNotRetriableButIsRecognised(t *testing.T) {
	err := refused(429, "slow down")

	assert.False(t, IsRetriable(err), "a rate limit must back off rather than retry")

	assert.True(t, IsRateLimited(err))

	assert.False(t, IsRateLimited(refused(500, "x")), "only a 429 is a rate limit")
	assert.False(t, IsRateLimited(errors.New("x")), "only a 429 is a rate limit")
}

func TestIsProviderErrorTellsARefusalFromACancellation(t *testing.T) {
	assert.True(t, IsProviderError(refused(500, "x")), "a provider refusal is a provider error")

	assert.True(t, IsProviderError(fmt.Errorf("wrapped: %w", refused(500, "x"))), "a wrapped provider error is still one")

	assert.False(t, IsProviderError(errors.New("context canceled")))
}

func TestRetryAfterReadsBothHeaderForms(t *testing.T) {
	withHeader := func(value string) error {
		return &fantasy.ProviderError{StatusCode: 429, ResponseHeaders: map[string]string{"retry-after": value}}
	}

	delay, ok := RetryAfter(withHeader("7"))
	assert.True(t, ok)
	assert.Equal(t, 7*time.Second, delay)

	future := time.Now().Add(30 * time.Second).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")

	delay, ok = RetryAfter(withHeader(future))
	assert.True(t, ok, "date form = %v, %v, want about 30s", delay, ok)
	assert.GreaterOrEqual(t, delay, 25*time.Second, "date form = %v, %v, want about 30s", delay, ok)
	assert.LessOrEqual(t, delay, 31*time.Second, "date form = %v, %v, want about 30s", delay, ok)

	// already past, or zero seconds. Advice to retry now, which is not no advice
	delay, ok = RetryAfter(withHeader("0"))
	assert.True(t, ok)
	assert.EqualValues(t, 0, delay)

	delay, ok = RetryAfter(withHeader("Mon, 02 Jan 2006 15:04:05 GMT"))
	assert.True(t, ok, "past date = %v, %v, want 0 with advice", delay, ok)
	assert.EqualValues(t, 0, delay, "past date = %v, %v, want 0 with advice", delay, ok)

	// no header, garbage, or not a provider error. No advice at all
	for _, err := range []error{refused(429, "x"), withHeader("soon"), withHeader(""), errors.New("x")} {
		_, ok := RetryAfter(err)
		assert.False(t, ok, "%v should carry no advice", err)
	}
}

// A count of seconds too large for the nanosecond arithmetic would wrap negative
// and slip under every "longer than the cap" check, stripping the backoff to
// nothing. It must saturate instead.
func TestAHugeRetryAfterSaturatesRatherThanOverflowing(t *testing.T) {
	err := &fantasy.ProviderError{StatusCode: 429, ResponseHeaders: map[string]string{"Retry-After": "99999999999999"}}

	delay, ok := RetryAfter(err)
	assert.True(t, ok)
	assert.Positive(t, delay)
}

func TestDetectContextLimitExtractsTheRealWindow(t *testing.T) {
	err := refused(400, "This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.")

	limit, ok := DetectContextLimit(err)
	require.True(t, ok, "a length rejection was not detected")

	assert.Equal(t, 8192, limit.MaxTokens, "want the window and usage the provider stated")
	assert.Equal(t, 9000, limit.UsedTokens, "want the window and usage the provider stated")

	// the retry has to leave room for the answer, so it aims below the window
	assert.Positive(t, limit.SuggestedLimit, "suggested = %d, want a positive budget under the %d window", limit.SuggestedLimit, limit.MaxTokens)
	assert.Less(t, limit.SuggestedLimit, limit.MaxTokens, "suggested = %d, want a positive budget under the %d window", limit.SuggestedLimit, limit.MaxTokens)
}

func TestDetectContextLimitTrustsWhatFantasyParsed(t *testing.T) {
	err := &fantasy.ProviderError{
		StatusCode: 400, Message: "too big", ContextTooLargeErr: true, ContextMaxTokens: 4096, ContextUsedTokens: 9000,
	}

	limit, ok := DetectContextLimit(err)
	assert.True(t, ok, "want the numbers fantasy extracted")
	assert.Equal(t, 4096, limit.MaxTokens, "want the numbers fantasy extracted")
	assert.Equal(t, 9000, limit.UsedTokens, "want the numbers fantasy extracted")
}

func TestDetectContextLimitRecognisesLlamaCpp(t *testing.T) {
	err := refused(400, "the request exceeds the available context size, try increasing it")

	limit, ok := DetectContextLimit(err)
	require.True(t, ok, "llama.cpp's wording was not recognized as a length rejection")

	assert.Equal(t, 0, limit.SuggestedLimit, "want none when no window was stated")
}

func TestDetectContextLimitIgnoresUnrelatedErrors(t *testing.T) {
	for _, err := range []error{nil, refused(401, "bad key"), errors.New("connection reset"), refused(400, "model not found")} {
		_, ok := DetectContextLimit(err)
		assert.False(t, ok, "%v is not a length rejection", err)
	}
}

// The SDK hands back the whole dumped response when it cannot parse one, status
// line and headers included. Only the body is evidence.
func TestFailureOfCarriesTheBodyNotTheDump(t *testing.T) {
	dump := "HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\n\r\n{\"error\":\"nope\"}"

	err := &fantasy.ProviderError{StatusCode: 400, ResponseBody: []byte(dump), RequestBody: []byte(`{"model":"m"}`)}

	failure := FailureOf(fmt.Errorf("run: %w", err))
	require.NotNil(t, failure, "a refusal carries evidence")

	assert.Equal(t, 400, failure.Status)
	assert.JSONEq(t, `{"error":"nope"}`, failure.ResponseBody)

	assert.Equal(t, len(`{"model":"m"}`), failure.RequestBytes, "want the size of what was refused")
}

func TestFailureOfIsAbsentWithoutAStatus(t *testing.T) {
	for _, err := range []error{nil, errors.New("canceled"), &fantasy.ProviderError{Message: "cut connection"}} {
		assert.Nil(t, FailureOf(err), "%v carries no wire evidence", err)
	}
}

func TestFailureOfBoundsWhatItKeeps(t *testing.T) {
	err := &fantasy.ProviderError{StatusCode: 500, ResponseBody: []byte(strings.Repeat("x", maxDumpBody+100))}

	failure := FailureOf(err)

	assert.LessOrEqual(t, len(failure.ResponseBody), maxDumpBody+len("…"), "want the body bounded")
}
