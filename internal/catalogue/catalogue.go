// Package catalogue records what the engine needs to know about a model before
// it calls one: how much context it has, how much of that it may spend on the
// answer, and whether it can use tools or emits a reasoning channel.
//
// None of it can be asked of the provider - the OpenAI-compatible
// chat-completions API has no endpoint that reports a model's limits - so it is
// kept here, with a conservative default for anything unrecognised.
package catalogue

import (
	"strings"
)

// Model describes a model's capabilities.
type Model struct {
	// ContextWindow is the total token budget for input plus output.
	ContextWindow int

	// MaxOutputTokens bounds a single response.
	MaxOutputTokens int
}

// DefaultContextWindow is assumed for models the catalogue has not heard of.
//
// Generous, because the failure modes are asymmetric in practice: assuming too
// small trims a long run over and over, dropping history the model could have
// kept, while assuming too large is recovered reactively - a provider's
// context-length rejection is detected and lowers the budget to what the error
// stated (see the loop's DetectContextLimit path), and the operator can pin
// the real ceiling per model with `context:` in the config. Frontier models
// the catalogue has not heard of are the ones most likely to carry windows
// this large.
const DefaultContextWindow = 1_048_576

// DefaultMaxOutputTokens is the output reserve assumed for unknown models.
// Not a quarter of the window: a quarter of a million-token window would
// reserve more room for the answer than most models can produce, squeezing
// input for nothing.
const DefaultMaxOutputTokens = 65_536

// Default is returned for unknown models.
var Default = Model{
	ContextWindow:   DefaultContextWindow,
	MaxOutputTokens: DefaultMaxOutputTokens,
}

// models is the catalogue.
//
// Entries are matched exactly first, then by longest prefix - so `gpt-5.4` names
// a specific model, and `gpt-5.4-mini-2026-03-01` resolves through it rather
// than dropping to the default. That is why there is one table and not two:
// providers ship dated and point-release variants constantly, and a name is
// either an entry or a variant of one.
//
// @note there are deliberately no realtime models. Realtime is a WebSocket audio
// protocol, not a chat-completions model, and nothing in a coding loop could
// drive one.
var models = map[string]Model{
	// ---------------------------------------------------------------- OpenAI

	"gpt-5.6-sol":   {ContextWindow: 1_050_000, MaxOutputTokens: 128_000},
	"gpt-5.6-terra": {ContextWindow: 1_050_000, MaxOutputTokens: 128_000},
	"gpt-5.6-luna":  {ContextWindow: 1_050_000, MaxOutputTokens: 128_000},
	"gpt-5.5":       {ContextWindow: 1_050_000, MaxOutputTokens: 128_000},
	"gpt-5.4":       {ContextWindow: 1_050_000, MaxOutputTokens: 128_000},
	"gpt-5.4-pro":   {ContextWindow: 1_050_000, MaxOutputTokens: 128_000},
	"gpt-5.4-mini":  {ContextWindow: 400_000, MaxOutputTokens: 128_000},
	"gpt-5.4-nano":  {ContextWindow: 400_000, MaxOutputTokens: 128_000},
	"gpt-5.2":       {ContextWindow: 400_000, MaxOutputTokens: 128_000},
	"gpt-5.1":       {ContextWindow: 400_000, MaxOutputTokens: 128_000},
	"gpt-5":         {ContextWindow: 400_000, MaxOutputTokens: 128_000},
	"gpt-5-mini":    {ContextWindow: 400_000, MaxOutputTokens: 128_000},
	"gpt-5-nano":    {ContextWindow: 400_000, MaxOutputTokens: 128_000},
	"o4-mini":       {ContextWindow: 200_000, MaxOutputTokens: 100_000},
	"o3-mini":       {ContextWindow: 200_000, MaxOutputTokens: 100_000},
	"o3":            {ContextWindow: 200_000, MaxOutputTokens: 100_000},

	// ------------------------------------------------------------- Anthropic

	"claude-5-opus":     {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"claude-5-sonnet":   {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"claude-5-haiku":    {ContextWindow: 200_000, MaxOutputTokens: 64_000},
	"claude-4.8-opus":   {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"claude-4.7-opus":   {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"claude-4.6-opus":   {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"claude-4.6-sonnet": {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"claude-4.5-opus":   {ContextWindow: 200_000, MaxOutputTokens: 64_000},
	"claude-4.5-sonnet": {ContextWindow: 1_000_000, MaxOutputTokens: 64_000},
	"claude-4.5-haiku":  {ContextWindow: 200_000, MaxOutputTokens: 64_000},
	"claude-4.1-opus":   {ContextWindow: 200_000, MaxOutputTokens: 32_000},
	"claude-4-opus":     {ContextWindow: 200_000, MaxOutputTokens: 8_192},
	"claude-4-sonnet":   {ContextWindow: 1_000_000, MaxOutputTokens: 8_192},

	// @note broad fallbacks, so an unrecognised Claude release lands somewhere
	// sensible rather than on the default

	"claude-opus":   {ContextWindow: 200_000, MaxOutputTokens: 32_000},
	"claude-sonnet": {ContextWindow: 200_000, MaxOutputTokens: 64_000},
	"claude-haiku":  {ContextWindow: 200_000, MaxOutputTokens: 64_000},
	"claude":        {ContextWindow: 200_000, MaxOutputTokens: 32_000},

	// ---------------------------------------------------------------- Google

	"gemini-3.6-flash":      {ContextWindow: 1_000_000, MaxOutputTokens: 64_000},
	"gemini-3.5-flash":      {ContextWindow: 1_000_000, MaxOutputTokens: 64_000},
	"gemini-3.1-pro":        {ContextWindow: 1_000_000, MaxOutputTokens: 64_000},
	"gemini-3.1-flash-lite": {ContextWindow: 1_000_000, MaxOutputTokens: 65_000},
	"gemini-3-flash":        {ContextWindow: 1_000_000, MaxOutputTokens: 64_000},
	"gemini-3-pro":          {ContextWindow: 1_048_576, MaxOutputTokens: 65_536},
	"gemini-2.5-pro":        {ContextWindow: 1_048_576, MaxOutputTokens: 8_192},
	"gemini-2.5-flash":      {ContextWindow: 1_000_000, MaxOutputTokens: 65_536},
	"gemini-2.5-flash-lite": {ContextWindow: 1_048_576, MaxOutputTokens: 65_535},
	"gemini":                {ContextWindow: 1_000_000, MaxOutputTokens: 64_000},
	"gemma-4-31b":           {ContextWindow: 262_144, MaxOutputTokens: 65_536},
	"gemma":                 {ContextWindow: 262_144, MaxOutputTokens: 65_536},

	// -------------------------------------------------------------------- ZAI

	"glm-5.3":       {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"glm-5.2":       {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"glm-5.1":       {ContextWindow: 202_000, MaxOutputTokens: 64_000},
	"glm-5-turbo":   {ContextWindow: 202_800, MaxOutputTokens: 128_000},
	"glm-5":         {ContextWindow: 202_800, MaxOutputTokens: 64_000},
	"glm-4.7-flash": {ContextWindow: 200_000, MaxOutputTokens: 128_000},
	"glm-4.7":       {ContextWindow: 200_000, MaxOutputTokens: 40_000},
	"glm":           {ContextWindow: 200_000, MaxOutputTokens: 64_000},

	// --------------------------------------------------------------- Moonshot

	"kimi-k3":        {ContextWindow: 1_000_000, MaxOutputTokens: 131_072},
	"kimi-k2.7-code": {ContextWindow: 256_000, MaxOutputTokens: 32_768},
	"kimi-k2.6":      {ContextWindow: 262_000, MaxOutputTokens: 65_500},
	"kimi-k2.5":      {ContextWindow: 262_144, MaxOutputTokens: 65_535},
	"kimi":           {ContextWindow: 262_000, MaxOutputTokens: 65_500},

	// ----------------------------------------------------------------- MiniMax

	"minimax-m3":   {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"minimax-m2.7": {ContextWindow: 204_800, MaxOutputTokens: 131_000},
	"minimax-m2.5": {ContextWindow: 204_800, MaxOutputTokens: 51_200},
	"minimax":      {ContextWindow: 204_800, MaxOutputTokens: 51_200},

	// -------------------------------------------------------------------- Qwen

	"qwen-3.8-max":  {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	"qwen-3.7-max":  {ContextWindow: 991_000, MaxOutputTokens: 64_000},
	"qwen-3.6-max":  {ContextWindow: 240_000, MaxOutputTokens: 64_000},
	"qwen-3.6-plus": {ContextWindow: 1_000_000, MaxOutputTokens: 64_000},
	"qwen":          {ContextWindow: 240_000, MaxOutputTokens: 64_000},

	// ---------------------------------------------------------------- DeepSeek

	"deepseek-v4-pro":   {ContextWindow: 1_048_600, MaxOutputTokens: 65_536},
	"deepseek-v4-flash": {ContextWindow: 1_000_000, MaxOutputTokens: 65_536},
	"deepseek-v3.2":     {ContextWindow: 164_000, MaxOutputTokens: 32_768},
	"deepseek-r":        {ContextWindow: 128_000, MaxOutputTokens: 32_768},
	"deepseek":          {ContextWindow: 164_000, MaxOutputTokens: 32_768},

	// -------------------------------------------------------------------- Xiaomi

	"mimo-v2.5-pro": {ContextWindow: 1_050_000, MaxOutputTokens: 131_000},
	"mimo-v2.5":     {ContextWindow: 1_050_000, MaxOutputTokens: 131_000},
	"mimo":          {ContextWindow: 1_050_000, MaxOutputTokens: 131_000},

	// ---------------------------------------------------------------- Mistral

	"mistral-large": {ContextWindow: 256_000, MaxOutputTokens: 64_000},
	"mistral-small": {ContextWindow: 32_000, MaxOutputTokens: 4_000},
	"mistral":       {ContextWindow: 128_000, MaxOutputTokens: 16_384},
	"devstral-2":    {ContextWindow: 256_000, MaxOutputTokens: 64_000},
	"devstral":      {ContextWindow: 256_000, MaxOutputTokens: 64_000},
	"codestral":     {ContextWindow: 256_000, MaxOutputTokens: 64_000},

	// --------------------------------------------------------------------- xAI

	"grok-4.5": {ContextWindow: 500_000, MaxOutputTokens: 128_000},
	"grok":     {ContextWindow: 256_000, MaxOutputTokens: 32_768},

	// ------------------------------------------------------------------- Meta

	"llama-4": {ContextWindow: 1_000_000, MaxOutputTokens: 16_384},
	"llama-3": {ContextWindow: 128_000, MaxOutputTokens: 8_192},
	"llama":   {ContextWindow: 128_000, MaxOutputTokens: 8_192},

	// ------------------------------------------------------------- Perplexity
	//
	// @note search-augmented models take no tools, so they cannot drive the
	// agentic loop. Listed so a caller finds out before the run rather than from
	// a provider error.

	"sonar-reasoning-pro": {ContextWindow: 127_000, MaxOutputTokens: 8_000},
	"sonar-reasoning":     {ContextWindow: 127_000, MaxOutputTokens: 8_000},
	"sonar-pro":           {ContextWindow: 200_000, MaxOutputTokens: 64_000},
	"sonar":               {ContextWindow: 127_000, MaxOutputTokens: 8_000},
}

// normalize reduces a model string to the name a catalogue entry would use.
func normalize(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))

	// a provider-qualified name ("openrouter/z-ai/glm-5.2") carries the model
	// after the last slash
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}

	// a deployment suffix ("gpt-5.4@2026-03-01") is not part of the identity
	if index := strings.Index(name, "@"); index >= 0 {
		name = name[:index]
	}

	return name
}

// Lookup returns what is known about a model.
//
// An exact entry wins; otherwise the longest matching prefix; otherwise Default.
// A caller never has to check whether a model is known - an unrecognised one is
// budgeted conservatively rather than refused, because declining to run against
// a model released last week would be worse than being careful with it.
func Lookup(model string) Model {
	name := normalize(model)

	if entry, ok := models[name]; ok {
		return entry
	}

	best := ""

	for prefix := range models {
		if strings.HasPrefix(name, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}

	if best == "" {
		return Default
	}

	return models[best]
}

// InputBudget is how many tokens of conversation may be sent to a model.
//
// It is the number the thread builder trims to,
// and it is not the same as the context window: the window has to hold the
// answer as well as the prompt. Filling it entirely leaves the model no room to
// reply, which does not present as a budgeting mistake - it presents as an empty
// or truncated turn, and the loop then spends its continuation budget retrying
// something that cannot succeed.
//
// So the output allowance is subtracted, and at least half the window is kept
// for input regardless, so a model advertising an enormous output ceiling cannot
// squeeze the conversation to nothing.
func (m Model) InputBudget() int {
	budget := m.ContextWindow - m.MaxOutputTokens

	if budget < m.ContextWindow/2 {
		budget = m.ContextWindow / 2
	}

	return budget
}

// InputBudget is the package-level convenience for a model name.
func InputBudget(model string) int {
	return Lookup(model).InputBudget()
}
