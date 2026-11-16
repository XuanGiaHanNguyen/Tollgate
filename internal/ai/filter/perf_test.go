package filter

import (
	"strings"
	"testing"
	"time"
)

// Worst cases must stay linear: long runs of combining marks (NFKC
// buffering), long partial-match prefixes, and 1-byte chunks.
func TestNoPathologicalSlowdown(t *testing.T) {
	s, _ := Compile("p", []string{"aaaaaaaaab", "password", "é"})
	cases := map[string]string{
		"combining marks":  "a" + strings.Repeat("́", 50_000),
		"partial prefixes": strings.Repeat("a", 100_000),
		"plain text":       strings.Repeat("the quick brown fox ", 5_000),
	}
	for name, text := range cases {
		for _, chunk := range []int{1, 7, 4096} {
			start := time.Now()
			st := s.NewStream(Redact)
			for i := 0; i < len(text); i += chunk {
				st.Write(text[i:min(i+chunk, len(text))])
			}
			st.Close()
			if el := time.Since(start); el > 2*time.Second {
				t.Errorf("%s, chunk %d: %v for %d bytes", name, chunk, el, len(text))
			} else {
				t.Logf("%s, chunk %d: %v for %d bytes", name, chunk, el, len(text))
			}
		}
	}
}
