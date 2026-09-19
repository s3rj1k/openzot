package loop

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
)

// refused is an error as fantasy reports one: a status and the provider's words.
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
		if got := IsRetriable(test.err); got != test.want {
			t.Errorf("%s: IsRetriable = %v, want %v", test.name, got, test.want)
		}
	}
}

// With no status to go by, what the error is decides: a stream that ended
// mid-turn, a reset connection and zot's own stall are transient however they
// are worded around.
func TestIsRetriableRecognisesTransportFailuresByType(t *testing.T) {
	reset := &url.Error{Op: "Post", URL: "http://gw", Err: &net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}

	for name, err := range map[string]error{
		"a stream cut short":  &fantasy.ProviderError{Message: "stream transport error", Cause: io.ErrUnexpectedEOF},
		"a bare EOF":          fmt.Errorf("read: %w", io.EOF),
		"a connection reset":  reset,
		"a stall":             fmt.Errorf("%w: nothing arrived for 10m", errStreamStalled),
		"a wrapped stall":     fmt.Errorf("run: %w", fmt.Errorf("%w: nothing arrived", errStreamStalled)),
		"a broken pipe":       &url.Error{Op: "Post", URL: "http://gw", Err: &net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)}},
		"a closed connection": &url.Error{Op: "Post", URL: "http://gw", Err: &net.OpError{Op: "write", Err: net.ErrClosed}},
	} {
		if !IsRetriable(err) {
			t.Errorf("%s should be retriable: %v", name, err)
		}
	}
}

// The other half of going by type: words alone decide nothing. These read like
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
		if IsRetriable(err) {
			t.Errorf("%s should not be retriable: %v", name, err)
		}
	}
}

// fantasy flags a failure delivered inside an open stream as transient. It has
// no status, and it is the shape of "the generation failed, ask again".
func TestATransientErrorWithNoStatusIsRetriable(t *testing.T) {
	err := &fantasy.ProviderError{Message: "the upstream fell over", TransientError: true}

	if !IsRetriable(err) {
		t.Error("a transient failure with no status should retry")
	}
}

func TestRateLimitIsNotRetriableButIsRecognised(t *testing.T) {
	err := refused(429, "slow down")

	if IsRetriable(err) {
		t.Error("a rate limit must back off rather than retry")
	}

	if !IsRateLimited(err) {
		t.Error("a 429 is a rate limit")
	}

	if IsRateLimited(refused(500, "x")) || IsRateLimited(errors.New("x")) {
		t.Error("only a 429 is a rate limit")
	}
}

func TestIsProviderErrorTellsARefusalFromACancellation(t *testing.T) {
	if !IsProviderError(refused(500, "x")) {
		t.Error("a provider refusal is a provider error")
	}

	if !IsProviderError(fmt.Errorf("wrapped: %w", refused(500, "x"))) {
		t.Error("a wrapped provider error is still one")
	}

	if IsProviderError(errors.New("context canceled")) {
		t.Error("a local error is not a provider error")
	}
}

func TestRetryAfterReadsBothHeaderForms(t *testing.T) {
	withHeader := func(value string) error {
		return &fantasy.ProviderError{StatusCode: 429, ResponseHeaders: map[string]string{"retry-after": value}}
	}

	if delay, ok := RetryAfter(withHeader("7")); !ok || delay != 7*time.Second {
		t.Errorf("seconds form = %v, %v, want 7s", delay, ok)
	}

	future := time.Now().Add(30 * time.Second).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")

	if delay, ok := RetryAfter(withHeader(future)); !ok || delay < 25*time.Second || delay > 31*time.Second {
		t.Errorf("date form = %v, %v, want about 30s", delay, ok)
	}

	// already past, or zero seconds: advice to retry now, which is not no advice
	if delay, ok := RetryAfter(withHeader("0")); !ok || delay != 0 {
		t.Errorf("zero = %v, %v, want 0 with advice", delay, ok)
	}

	if delay, ok := RetryAfter(withHeader("Mon, 02 Jan 2006 15:04:05 GMT")); !ok || delay != 0 {
		t.Errorf("past date = %v, %v, want 0 with advice", delay, ok)
	}

	// no header, garbage, or not a provider error: no advice at all
	for _, err := range []error{refused(429, "x"), withHeader("soon"), withHeader(""), errors.New("x")} {
		if _, ok := RetryAfter(err); ok {
			t.Errorf("%v should carry no advice", err)
		}
	}
}

// A count of seconds too large for the nanosecond arithmetic would wrap negative
// and slip under every "longer than the cap" check, stripping the backoff to
// nothing. It must saturate instead.
func TestAHugeRetryAfterSaturatesRatherThanOverflowing(t *testing.T) {
	err := &fantasy.ProviderError{StatusCode: 429, ResponseHeaders: map[string]string{"Retry-After": "99999999999999"}}

	delay, ok := RetryAfter(err)
	if !ok || delay <= 0 {
		t.Errorf("delay = %v, %v, want a large positive delay", delay, ok)
	}
}

func TestDetectContextLimitExtractsTheRealWindow(t *testing.T) {
	err := refused(400, "This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.")

	limit, ok := DetectContextLimit(err)
	if !ok {
		t.Fatal("a length rejection was not detected")
	}

	if limit.MaxTokens != 8192 || limit.UsedTokens != 9000 {
		t.Errorf("limit = %+v, want the window and usage the provider stated", limit)
	}

	// the retry has to leave room for the answer, so it aims below the window
	if limit.SuggestedLimit <= 0 || limit.SuggestedLimit >= limit.MaxTokens {
		t.Errorf("suggested = %d, want a positive budget under the %d window", limit.SuggestedLimit, limit.MaxTokens)
	}
}

func TestDetectContextLimitTrustsWhatFantasyParsed(t *testing.T) {
	err := &fantasy.ProviderError{
		StatusCode: 400, Message: "too big", ContextTooLargeErr: true, ContextMaxTokens: 4096, ContextUsedTokens: 9000,
	}

	limit, ok := DetectContextLimit(err)
	if !ok || limit.MaxTokens != 4096 || limit.UsedTokens != 9000 {
		t.Errorf("limit = %+v, %v, want the numbers fantasy extracted", limit, ok)
	}
}

func TestDetectContextLimitRecognisesLlamaCpp(t *testing.T) {
	err := refused(400, "the request exceeds the available context size, try increasing it")

	limit, ok := DetectContextLimit(err)
	if !ok {
		t.Fatal("llama.cpp's wording was not recognised as a length rejection")
	}

	if limit.SuggestedLimit != 0 {
		t.Errorf("suggested = %d, want none when no window was stated", limit.SuggestedLimit)
	}
}

func TestDetectContextLimitIgnoresUnrelatedErrors(t *testing.T) {
	for _, err := range []error{nil, refused(401, "bad key"), errors.New("connection reset"), refused(400, "model not found")} {
		if _, ok := DetectContextLimit(err); ok {
			t.Errorf("%v is not a length rejection", err)
		}
	}
}

// The SDK hands back the whole dumped response when it cannot parse one, status
// line and headers included. Only the body is evidence.
func TestFailureOfCarriesTheBodyNotTheDump(t *testing.T) {
	dump := "HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\n\r\n{\"error\":\"nope\"}"

	err := &fantasy.ProviderError{StatusCode: 400, ResponseBody: []byte(dump), RequestBody: []byte(`{"model":"m"}`)}

	failure := FailureOf(fmt.Errorf("run: %w", err))
	if failure == nil {
		t.Fatal("a refusal carries evidence")
	}

	if failure.Status != 400 || failure.ResponseBody != `{"error":"nope"}` {
		t.Errorf("failure = %+v, want the status and only the body", failure)
	}

	if failure.RequestBytes != len(`{"model":"m"}`) {
		t.Errorf("request size = %d, want the size of what was refused", failure.RequestBytes)
	}
}

func TestFailureOfIsAbsentWithoutAStatus(t *testing.T) {
	for _, err := range []error{nil, errors.New("cancelled"), &fantasy.ProviderError{Message: "cut connection"}} {
		if FailureOf(err) != nil {
			t.Errorf("%v carries no wire evidence", err)
		}
	}
}

func TestFailureOfBoundsWhatItKeeps(t *testing.T) {
	err := &fantasy.ProviderError{StatusCode: 500, ResponseBody: []byte(strings.Repeat("x", maxDumpBody+100))}

	failure := FailureOf(err)

	if len(failure.ResponseBody) > maxDumpBody+len("…") {
		t.Errorf("kept %d bytes, want the body bounded", len(failure.ResponseBody))
	}
}
