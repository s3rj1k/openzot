package loop

// Token costs here are estimates, not counts. zot talks to providers whose
// tokenizers are private, change independently, or differ from OpenAI's
// vocabulary, and a model-specific vocabulary would make one provider look
// precise while adding several megabytes and staying an approximation
// everywhere else. So text is priced by its UTF-8 bytes, conservatively, and the
// estimate errs toward over-counting: a long run is trimmed a little early
// rather than rejected. Once a request succeeds, the run is billed on the
// provider's own reported usage.

// bytesPerToken deliberately sits below the roughly four bytes per token of
// ordinary English. Code, JSON and identifiers are denser, while UTF-8 makes
// non-ASCII scripts consume multiple bytes per rune.
const bytesPerToken = 3

// messageOverhead is the fixed allowance for the role and control data a chat
// wire format wraps around every message, none of which appears in its text.
// Deliberately generous.
const messageOverhead = 10

// estimateTokens prices text at one token per three UTF-8 bytes, then adds 25%
// headroom. Each division rounds up, because under-counting can make a provider
// reject a request whereas over-counting merely trims a little early.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}

	base := (len(text) + bytesPerToken - 1) / bytesPerToken

	return (base*5 + 3) / 4
}

// estimateMessageTokens is a message's text cost plus its wire envelope.
func estimateMessageTokens(text string) int {
	return estimateTokens(text) + messageOverhead
}
