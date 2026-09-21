package conversation

// Token costs are estimates, not counts, since provider tokenizers are private and differ. Text is priced by
// its UTF-8 bytes, conservatively, so a long run is trimmed a little early rather than rejected. Once a request
// succeeds, the run is billed on the provider's own reported usage.

// bytesPerToken by design sits below the roughly four bytes per token of
// ordinary English. Code, JSON and identifiers are denser, while UTF-8 makes
// non-ASCII scripts consume multiple bytes per rune.
const bytesPerToken = 3

// messageOverhead is the fixed allowance for the role and control data a chat
// wire format wraps around every message, none of which appears in its text.
// By design generous.
const messageOverhead = 10

// EstimateTokens prices text at one token per three UTF-8 bytes, then adds 25%
// headroom. Each division rounds up, because under-counting can make a provider
// reject a request whereas over-counting only trims a little early.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}

	base := (len(text) + bytesPerToken - 1) / bytesPerToken

	return (base*5 + 3) / 4
}

// BytesForTokens is roughly how many bytes of text a count of tokens stands for,
// by the same conservative measure the estimates use.
func BytesForTokens(tokens int) int {
	return tokens * bytesPerToken
}
