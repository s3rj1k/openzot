package llm

import (
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

// maxDumpBody bounds a captured request or response body. Generous, because a
// developer dump wants the whole exchange, but capped so a pathological
// request cannot pin arbitrary memory on the error that ends a run.
const maxDumpBody = 1 << 20 // 1 MiB

// retriablePatterns match transient failures that arrive without a usable
// status - a bare transport error, or a gateway that puts the real problem in
// the body of a 200.
//
// @note matching on prose is fragile: gateways word the same condition
// differently, and a wording the list does not anticipate turns a transient blip
// into a hard failure. Prefer the status; these are the fallback for errors that
// carry none.
var retriablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)provider returned error`),
	regexp.MustCompile(`(?i)internal server error`),
	regexp.MustCompile(`(?i)bad gateway`),
	// tolerate an adverb between the two words, as well as the bare form
	regexp.MustCompile(`(?i)service\s+(?:\w+\s+)?unavailable`),
	regexp.MustCompile(`(?i)temporarily unavailable`),
	regexp.MustCompile(`(?i)gateway timeout`),
	regexp.MustCompile(`(?i)\boverloaded\b`),
	regexp.MustCompile(`(?i)connection reset`),
	regexp.MustCompile(`(?i)EOF`),
	// zot's own wordings for a stream that died mid-turn
	regexp.MustCompile(`(?i)the stream (?:ended|stalled)`),
}

// stallPatterns match a stream that stopped producing - the upstream went
// quiet, rather than the request being wrong.
//
// These are the one wording allowed to outrank the status, because gateways
// report a stall with whatever status they please and at least one picks a 4xx.
// Deliberately narrow: a bare "timeout" is not enough, since "timeout must be a
// positive integer" is a rejected parameter and retrying it burns the budget.
var stallPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:idle|stream|read|upstream|inactivity)[ _-]?timeout\b`),
	regexp.MustCompile(`(?i)\btimeout exceeded\b`),
	regexp.MustCompile(`(?i)\b(?:request|upstream|connection) timed out\b`),
}

// trailingStatusPattern matches the status some providers append to a message.
// Worth recovering even when the error carries no status field: a status is
// authoritative where prose is not.
var trailingStatusPattern = regexp.MustCompile(`\((\d{3})\)\s*$`)

// providerError is the error fantasy raises for anything an endpoint did wrong.
func providerError(err error) (*fantasy.ProviderError, bool) {
	var found *fantasy.ProviderError

	return found, errors.As(err, &found)
}

// statusOf recovers the HTTP status behind an error, if it carries one.
func statusOf(err error) (int, bool) {
	if found, ok := providerError(err); ok && found.StatusCode != 0 {
		return found.StatusCode, true
	}

	if found := trailingStatusPattern.FindStringSubmatch(err.Error()); len(found) == 2 {
		status, convErr := strconv.Atoi(found[1])

		if convErr == nil && status >= 100 && status <= 599 {
			return status, true
		}
	}

	return 0, false
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
// The status is authoritative in both directions when present: a 5xx retries and
// a 4xx does not, whatever the message says. A 4xx is caused by the request
// itself - a bad key, a model the provider does not have - and retrying only
// burns the budget. Two carve-outs, both about time rather than the request: a
// 408, and a message that names a stalled stream. An error with no status that
// fantasy itself marks transient - an in-band error frame, a cut connection -
// retries too.
//
// 429 is deliberately excluded. A rate limit needs Retry-After backoff, not a
// tight retry loop, and retrying it aggressively makes the throttling worse.
func IsRetriable(err error) bool {
	if err == nil {
		return false
	}

	message := err.Error()

	for _, pattern := range stallPatterns {
		if pattern.MatchString(message) {
			return true
		}
	}

	if status, ok := statusOf(err); ok {
		return status == http.StatusRequestTimeout || (status >= 500 && status <= 599)
	}

	if found, ok := providerError(err); ok && found.IsRetryable() {
		return true
	}

	for _, pattern := range retriablePatterns {
		if pattern.MatchString(message) {
			return true
		}
	}

	return false
}

// IsRateLimited reports a 429, which the caller should back off from rather than
// retry immediately.
func IsRateLimited(err error) bool {
	found, ok := providerError(err)

	return ok && found.StatusCode == http.StatusTooManyRequests
}

// RetryAfter reports the delay the provider advised before trying again, and
// whether it advised one at all.
//
// This is the half of the rate-limit contract that makes excluding 429 from
// IsRetriable defensible: the caller does not loop, it waits for as long as the
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

// parseRetryAfter reads the two forms the Retry-After header takes: a count of
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

		// saturate rather than overflow: a count too large for the nanosecond
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

// contextLimitPatterns identify a prompt that exceeded the model's window.
//
// This is recoverable in a way most 4xx are not: the run can trim harder and try
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
// provider refuses a prompt for length it states the actual limit, and that is
// ground truth in a way the catalogue is not.
var maximumContextPattern = regexp.MustCompile(`(?i)maximum context length is\s+(\d+)\s+tokens`)

// usedTokensPattern pulls out what the rejected request actually cost.
var usedTokensPattern = regexp.MustCompile(`(?i)resulted in\s+(\d+)\s+tokens`)

// contextLimitSafetyRatio is how much of the stated window to aim for on retry.
// Not all of it: the window has to hold the answer too, and the rebuilt prompt
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

// Failure is the wire evidence behind a provider refusal.
type Failure struct {
	// Status is the HTTP status of the refusal.
	Status int

	// Body is the raw (bounded) response body.
	Body string

	// RequestBytes is the size of the request that was refused.
	RequestBytes int

	// RequestBody is the JSON that was refused, bounded.
	RequestBody string
}

// FailureOf extracts the wire evidence from an error, when it carries any.
func FailureOf(err error) (Failure, bool) {
	found, ok := providerError(err)
	if !ok || found.StatusCode == 0 {
		return Failure{}, false
	}

	return Failure{
		Status:       found.StatusCode,
		Body:         clip(bodyOf(found.ResponseBody), maxDumpBody),
		RequestBytes: len(found.RequestBody),
		RequestBody:  clip(string(found.RequestBody), maxDumpBody),
	}, true
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
