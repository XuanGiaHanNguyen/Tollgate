// Package tokens estimates LLM token counts and prices them. Estimates are
// deliberately simple (no model-specific tokenizer): about 4 bytes of text
// per token, and at least one token per word. Money is int64 micro-USD so
// budgets never accumulate float error.
package tokens

import (
	"encoding/json"
	"math"
	"strings"
	"unicode"
)

// Estimate approximates the token count of text.
func Estimate(text string) int64 {
	if text == "" {
		return 0
	}
	byBytes := int64(math.Ceil(float64(len(text)) / 4))
	words := int64(len(strings.FieldsFunc(text, unicode.IsSpace)))
	if words > byBytes {
		return words
	}
	return byBytes
}

// ChatRequest is the subset of an OpenAI-style chat request the gateway reads.
type ChatRequest struct {
	Model     string        `json:"model"`
	MaxTokens int64         `json:"max_tokens"`
	Stream    bool          `json:"stream"`
	Messages  []ChatMessage `json:"messages"`
}

// ChatMessage is one message; content may be a string or other JSON.
type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Text returns the message content as text (string content, or the raw
// JSON for structured content).
func (m ChatMessage) Text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	return string(m.Content)
}

// PromptTokens estimates the prompt size: every message's content plus a
// small per-message overhead for role and framing.
func (r *ChatRequest) PromptTokens() int64 {
	var n int64
	for _, m := range r.Messages {
		n += Estimate(m.Text()) + 4
	}
	return n
}

// MicroUSD is an amount of money in millionths of a dollar.
type MicroUSD = int64

// Price is a model's price per 1,000 tokens, in micro-USD.
type Price struct {
	InputPer1K  MicroUSD
	OutputPer1K MicroUSD
}

// Cost prices a token count, rounding up so estimates never undercount.
func (p Price) Cost(in, out int64) MicroUSD {
	return ceilDiv(in*p.InputPer1K, 1000) + ceilDiv(out*p.OutputPer1K, 1000)
}

func ceilDiv(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// USDToMicro converts dollars to micro-USD, rounding to the nearest unit.
func USDToMicro(usd float64) MicroUSD { return MicroUSD(math.Round(usd * 1e6)) }
