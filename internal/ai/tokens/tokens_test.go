package tokens

import (
	"encoding/json"
	"testing"
)

func TestEstimate(t *testing.T) {
	tests := map[string]int64{
		"":                     0,
		"hi":                   1,
		"hello world":          3, // 11 bytes -> ceil(11/4)=3 >= 2 words
		"a b c d e f g h":      8, // 8 words beat ceil(15/4)=4
		"supercalifragilistic": 5,
	}
	for in, want := range tests {
		if got := Estimate(in); got != want {
			t.Errorf("Estimate(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestPromptTokensAndContentShapes(t *testing.T) {
	var r ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"small","max_tokens":100,"messages":[
		{"role":"user","content":"hello world"},
		{"role":"user","content":[{"type":"text","text":"x"}]}]}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Model != "small" || r.MaxTokens != 100 {
		t.Fatalf("%+v", r)
	}
	if got := r.PromptTokens(); got < 3+4+4 {
		t.Fatalf("prompt tokens = %d", got)
	}
}

func TestCostRoundsUp(t *testing.T) {
	p := Price{InputPer1K: 500, OutputPer1K: 1500} // $0.0005 / $0.0015 per 1K
	if got := p.Cost(1000, 1000); got != 2000 {
		t.Fatalf("cost = %d", got)
	}
	if got := p.Cost(1, 1); got != 1+2 { // 0.5 -> 1, 1.5 -> 2
		t.Fatalf("cost = %d", got)
	}
	if USDToMicro(5.00) != 5_000_000 || USDToMicro(0.0000015) != 2 {
		t.Fatal("USDToMicro")
	}
}
