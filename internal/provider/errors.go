package provider

import (
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/fantasy"
)

// maxDumpBody bounds a captured request or response body. Generous, because a
// developer dump wants the whole exchange, but capped so a pathological
// request cannot pin arbitrary memory on the error that ends a run.
const maxDumpBody = 1 << 20 // 1 MiB

// providerError is the error fantasy raises for anything an endpoint did wrong.
func providerError(err error) (*fantasy.ProviderError, bool) {
	return errors.AsType[*fantasy.ProviderError](err)
}

// IsProviderError reports whether err is a failure the endpoint returned, as
// opposed to a local one (a cancellation, a context deadline). It is how an
// abort tells the exchange worth preserving from the bare cancellation that
// ended it.
func IsProviderError(err error) bool {
	_, ok := providerError(err)

	return ok
}

// IsRetriable reports whether an error is a transient provider failure worth
// retrying.
//
// It goes by what the error is, never by what it says. Gateways word the same
// condition differently, and matching prose turns a wording nobody anticipated
// into a run that ends on a blip. For an error the provider answered with, that is
// fantasy's own rule (ProviderError.IsRetryable). A 5xx, 408 or 409, an in-band
// error frame, a stream that ended mid-turn, an HTTP/2 reset, or a response that
// says so with x-should-retry. Anything else - a 4xx caused by the request itself,
// such as a bad key or a model the provider does not have - would only burn the
// budget. What fantasy's errors do not cover are the failures that never became a
// provider answer. A connection reset or closed under the request (however far
// the request had got - a peer that hangs up mid-write surfaces as a broken pipe
// or a closed connection, not a reset), a bare EOF, and zot's own stall.
//
// 429 is by design excluded, though fantasy would retry it. A rate limit needs
// Retry-After backoff, not a tight retry loop, and retrying it aggressively makes
// the throttling worse. The caller checks IsRateLimited first.
func IsRetriable(err error) bool {
	if err == nil {
		return false
	}

	if found, ok := providerError(err); ok {
		if found.StatusCode == http.StatusTooManyRequests {
			return false
		}

		if found.IsRetryable() {
			return true
		}
	}

	return errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, errStreamStalled)
}

// IsRateLimited reports a 429, which the caller should back off from rather than
// retry immediately.
func IsRateLimited(err error) bool {
	found, ok := providerError(err)

	return ok && found.StatusCode == http.StatusTooManyRequests
}

// parseRetryAfter reads the two forms the Retry-After header takes. A count of
// seconds, and an HTTP-date to wait until. A date already in the past is advice
// to retry now, not a negative sleep.
func parseRetryAfter(header string) (time.Duration, bool) {
	value := strings.TrimSpace(header)

	if value == "" {
		return 0, false
	}

	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0, true
		}

		// saturate rather than overflow. A count too large for the nanosecond
		// arithmetic would wrap negative and slip under the caller's cap
		if int64(seconds) > math.MaxInt64/int64(time.Second) {
			return time.Duration(math.MaxInt64), true
		}

		return time.Duration(seconds) * time.Second, true
	}

	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}

	if delay := time.Until(when); delay > 0 {
		return delay, true
	}

	return 0, true
}

// RetryAfter reports the delay the provider advised before trying again, and
// whether it advised one at all.
//
// This is the half of the rate-limit contract that makes excluding 429 from
// IsRetriable defensible. The caller does not loop, it waits for as long as the
// provider asked. A delay of zero with ok true means "now" rather than "no advice".
func RetryAfter(err error) (time.Duration, bool) {
	found, ok := providerError(err)
	if !ok {
		return 0, false
	}

	for key, value := range found.ResponseHeaders {
		if strings.EqualFold(key, "Retry-After") {
			return parseRetryAfter(value)
		}
	}

	return 0, false
}

// contextLimitPatterns identify a prompt that exceeded the model's window.
//
// This is recoverable in a way most 4xx are not. The run can trim harder and try
// again rather than failing. Detecting it needs prose because providers report
// it as a generic 400 with no distinguishing code.
var contextLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)context[ _-]?length`),
	regexp.MustCompile(`(?i)context window`),
	regexp.MustCompile(`(?i)maximum context`),
	regexp.MustCompile(`(?i)too many tokens`),
	regexp.MustCompile(`(?i)reduce the length`),
	regexp.MustCompile(`(?i)prompt is too long`),
	regexp.MustCompile(`(?i)input length and .* exceed`),
	// llama.cpp
	regexp.MustCompile(`(?i)exceeds the available context size`),
}

// maximumContextPattern pulls the real window out of the rejection. When a
// provider rejects a prompt for length it states the actual limit, and that is
// ground truth in a way the configured window may not be.
var maximumContextPattern = regexp.MustCompile(`(?i)maximum context length is\s+(\d+)\s+tokens`)

// usedTokensPattern pulls out what the rejected request actually cost.
var usedTokensPattern = regexp.MustCompile(`(?i)resulted in\s+(\d+)\s+tokens`)

// contextLimitSafetyRatio is how much of the stated window to aim for on retry.
// Not all of it. The window has to hold the answer too, and the rebuilt prompt
// carries slightly different overhead.
const contextLimitSafetyRatio = 0.85

// ContextLimit is what a length rejection revealed.
type ContextLimit struct {
	// MaxTokens is the window the provider stated, or zero if it did not.
	MaxTokens int

	// UsedTokens is what the rejected request cost, or zero if unstated.
	UsedTokens int

	// SuggestedLimit is the budget to retry under. Zero when the provider gave
	// no number, in which case the caller falls back to its own estimate.
	SuggestedLimit int
}

// DetectContextLimit reports whether a request was rejected for being too large,
// and what the provider said about it.
func DetectContextLimit(err error) (ContextLimit, bool) {
	if err == nil {
		return ContextLimit{}, false
	}

	message := err.Error()

	matched := false

	if found, ok := providerError(err); ok && found.IsContextTooLarge() {
		matched = true
	}

	for _, pattern := range contextLimitPatterns {
		if !matched && pattern.MatchString(message) {
			matched = true
		}
	}

	if !matched {
		return ContextLimit{}, false
	}

	limit := ContextLimit{}

	if found, ok := providerError(err); ok {
		limit.MaxTokens = found.ContextMaxTokens
		limit.UsedTokens = found.ContextUsedTokens
	}

	if found := maximumContextPattern.FindStringSubmatch(message); len(found) == 2 && limit.MaxTokens == 0 {
		if value, convErr := strconv.Atoi(found[1]); convErr == nil {
			limit.MaxTokens = value
		}
	}

	if found := usedTokensPattern.FindStringSubmatch(message); len(found) == 2 && limit.UsedTokens == 0 {
		if value, convErr := strconv.Atoi(found[1]); convErr == nil {
			limit.UsedTokens = value
		}
	}

	if limit.MaxTokens > 0 {
		limit.SuggestedLimit = int(float64(limit.MaxTokens) * contextLimitSafetyRatio)
	}

	return limit, true
}

// Failure is the wire evidence behind a provider rejection. It is recorded in the
// session log as it is, so its JSON tags are the log's schema.
type Failure struct {
	// Status is the HTTP status of the rejection.
	Status int `json:"status"`

	// ResponseBody is the raw (bounded) body the provider returned.
	ResponseBody string `json:"response_body,omitempty"`

	/*
		RequestBytes is the size of the request that was rejected - against a
		suspected context ceiling, the number that turns a correlation into a
		diagnosis.
	*/
	RequestBytes int `json:"request_bytes,omitzero"`
}

// bodyOf returns a response's body. The SDK hands back the whole dumped
// response, status line and headers included, when it could not parse one.
func bodyOf(dump []byte) string {
	text := strings.TrimSpace(string(dump))

	if strings.HasPrefix(text, "HTTP/") {
		if _, body, found := strings.Cut(text, "\r\n\r\n"); found {
			return strings.TrimSpace(body)
		}
	}

	return text
}

// clip bounds a body kept as evidence.
func clip(s string, limit int) string {
	if len(s) > limit {
		return s[:limit] + "…"
	}

	return s
}

// FailureOf extracts the wire evidence from an error, when it carries any.
func FailureOf(err error) *Failure {
	found, ok := providerError(err)
	if !ok || found.StatusCode == 0 {
		return nil
	}

	return &Failure{
		Status:       found.StatusCode,
		ResponseBody: clip(bodyOf(found.ResponseBody), maxDumpBody),
		RequestBytes: len(found.RequestBody),
	}
}
