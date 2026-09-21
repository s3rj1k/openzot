package conversation

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEstimateTokensPricesRepresentativeInput(t *testing.T) {
	for _, text := range []string{
		"hello",
		"Hello world, this is ordinary prose.",
		`{"tool_calls":[{"name":"shell","arguments":{"command":"go test ./..."}}]}`,
		"４日 동안 비가 내렸다. 안녕하세요 여러분",
		strings.Repeat("identifier", 100),
	} {
		assert.Positive(t, EstimateTokens(text), "want a positive estimate")
	}

	assert.Equal(t, 0, EstimateTokens(""))
}

// Non-ASCII scripts take several bytes per rune, and a tokenizer that sees a
// character as one unit would badly under-price them. Pricing by bytes keeps the
// estimate above the rune count.
func TestNonASCIIInputIsPricedByUTF8Bytes(t *testing.T) {
	text := "你好世界 안녕하세요"

	got, runes := EstimateTokens(text), utf8.RuneCountInString(text)
	assert.GreaterOrEqual(t, got, runes, "estimate = %d, want at least %d for non-ASCII input", got, runes)
}

// An under-count makes a provider reject a request, and an over-count only
// trims a little early, so the estimate has to sit above what ordinary English
// costs (about four bytes a token), not at it.
func TestEstimateIsConservativeForDenseASCII(t *testing.T) {
	text := strings.Repeat("a", 120)

	got, englishCost := EstimateTokens(text), len(text)/4
	assert.Greater(t, got, englishCost, "estimate = %d, want a safety margin above the %d ordinary English would cost", got, englishCost)
}

func TestEstimateGrowsWithTheText(t *testing.T) {
	base := "The quick brown fox jumps over the lazy dog. "
	previous := 0

	for repeat := 1; repeat <= 20; repeat++ {
		got := EstimateTokens(strings.Repeat(base, repeat))
		require.Greater(t, got, previous, "%d repeats estimated %d, not more than %d", repeat, got, previous)

		previous = got
	}
}

// Every message pays for its envelope whatever it says, so an empty one still
// costs something and a spoken one costs more than its text alone.
func TestAMessageCostsMoreThanItsText(t *testing.T) {
	const text = "a short message"

	got, bare := Cost(Message{Text: text}), EstimateTokens(text)
	assert.Greater(t, got, bare, "message estimate = %d, want more than the bare text's %d", got, bare)

	assert.Positive(t, Cost(Message{}), "want its envelope priced")
}

// A provider's control sequences are ordinary text in a conversation, and one in
// a tool result must be priced like any other bytes rather than treated as free.
func TestLiteralControlSequencesRemainPriced(t *testing.T) {
	for _, text := range []string{"<|im_end|>", "<|endoftext|>", "<|endofprompt|>"} {
		assert.GreaterOrEqual(t, EstimateTokens(text), 2, "want literal text priced")
	}
}
