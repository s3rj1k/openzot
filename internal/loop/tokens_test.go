package loop

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEstimateTokensPricesRepresentativeInput(t *testing.T) {
	for _, text := range []string{
		"hello",
		"Hello world, this is ordinary prose.",
		`{"tool_calls":[{"name":"shell","arguments":{"command":"go test ./..."}}]}`,
		"４日 동안 비가 내렸다. 안녕하세요 여러분",
		strings.Repeat("identifier", 100),
	} {
		if got := estimateTokens(text); got <= 0 {
			t.Errorf("estimateTokens(%.30q) = %d, want a positive estimate", text, got)
		}
	}

	if got := estimateTokens(""); got != 0 {
		t.Errorf("estimateTokens(empty) = %d, want 0", got)
	}
}

// Non-ASCII scripts take several bytes per rune, and a tokenizer that sees a
// character as one unit would badly under-price them. Pricing by bytes keeps the
// estimate above the rune count.
func TestNonASCIIInputIsPricedByUTF8Bytes(t *testing.T) {
	text := "你好世界 안녕하세요"

	if got, runes := estimateTokens(text), utf8.RuneCountInString(text); got < runes {
		t.Errorf("estimate = %d, want at least %d for non-ASCII input", got, runes)
	}
}

// An under-count makes a provider reject a request, and an over-count merely
// trims a little early, so the estimate has to sit above what ordinary English
// costs (about four bytes a token), not at it.
func TestEstimateIsConservativeForDenseASCII(t *testing.T) {
	text := strings.Repeat("a", 120)

	if got, englishCost := estimateTokens(text), len(text)/4; got <= englishCost {
		t.Errorf("estimate = %d, want a safety margin above the %d ordinary English would cost", got, englishCost)
	}
}

func TestEstimateGrowsWithTheText(t *testing.T) {
	base := "The quick brown fox jumps over the lazy dog. "
	previous := 0

	for repeat := 1; repeat <= 20; repeat++ {
		got := estimateTokens(strings.Repeat(base, repeat))
		if got <= previous {
			t.Fatalf("%d repeats estimated %d, not more than %d", repeat, got, previous)
		}

		previous = got
	}
}

// Every message pays for its envelope whatever it says, so an empty one still
// costs something and a spoken one costs more than its text alone.
func TestAMessageCostsMoreThanItsText(t *testing.T) {
	const text = "a short message"

	if got, bare := estimateMessageTokens(text), estimateTokens(text); got <= bare {
		t.Errorf("message estimate = %d, want more than the bare text's %d", got, bare)
	}

	if got := estimateMessageTokens(""); got <= 0 {
		t.Errorf("an empty message estimated %d, want its envelope priced", got)
	}
}

// A provider's control sequences are ordinary text in a conversation, and one in
// a tool result must be priced like any other bytes rather than treated as free.
func TestLiteralControlSequencesRemainPriced(t *testing.T) {
	for _, text := range []string{"<|im_end|>", "<|endoftext|>", "<|endofprompt|>"} {
		if got := estimateTokens(text); got < 2 {
			t.Errorf("estimateTokens(%q) = %d, want literal text priced", text, got)
		}
	}
}
