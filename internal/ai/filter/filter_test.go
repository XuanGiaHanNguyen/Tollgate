package filter

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func mustSet(t testing.TB, patterns ...string) *Set {
	t.Helper()
	s, err := Compile("test", patterns)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// run streams text in the given chunk sizes and returns the full output.
func run(t testing.TB, s *Set, a Action, text string, sizes []int) (string, int, error) {
	t.Helper()
	st := s.NewStream(a)
	var out strings.Builder
	rest := text
	for _, n := range sizes {
		if n > len(rest) {
			n = len(rest)
		}
		o, err := st.Write(rest[:n])
		out.WriteString(o)
		if err != nil {
			return out.String(), st.Matches(), err
		}
		rest = rest[n:]
	}
	o, err := st.Write(rest)
	out.WriteString(o)
	if err != nil {
		return out.String(), st.Matches(), err
	}
	o, err = st.Close()
	out.WriteString(o)
	return out.String(), st.Matches(), err
}

func TestScanBasics(t *testing.T) {
	s := mustSet(t, "he", "she", "his", "hers")
	got := s.Scan("ushers")
	// classic Aho-Corasick example: she@1, he@2, hers@2
	if len(got) != 3 {
		t.Fatalf("matches = %+v", got)
	}
}

func TestCaseAndNFKC(t *testing.T) {
	s := mustSet(t, "home address")
	for _, text := range []string{
		"my HOME ADDRESS is",
		"my Ｈｏｍｅ Ａｄｄｒｅｓｓ is", // full-width look-alikes
		"my home address is",      // no-break space normalizes to space
	} {
		if len(s.Scan(text)) != 1 {
			t.Errorf("missed in %q", text)
		}
	}
	if len(s.Scan("my homeaddress")) != 0 {
		t.Error("false positive")
	}
}

func TestRedactAcrossChunkBoundary(t *testing.T) {
	s := mustSet(t, "secret code")
	out, n, err := run(t, s, Redact, "the secret code is 42", []int{5, 4, 3, 1, 1})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if out != "the *********** is 42" {
		t.Fatalf("out = %q", out)
	}
}

func TestHoldbackNeverReleasesAPrefixEarly(t *testing.T) {
	s := mustSet(t, "abcdef")
	st := s.NewStream(Redact)
	o1, _ := st.Write("xxabc")
	if strings.Contains(o1, "abc") {
		t.Fatalf("released %q before the pattern could complete", o1)
	}
	o2, _ := st.Write("def!")
	o3, _ := st.Close()
	if got := o1 + o2 + o3; got != "xx******!" {
		t.Fatalf("out = %q", got)
	}
}

func TestBlockAndFlag(t *testing.T) {
	s := mustSet(t, "bad word")
	out, _, err := run(t, s, Block, "fine text then bad word and more", []int{10, 7, 3})
	if err != ErrBlocked || strings.Contains(out, "bad") || strings.Contains(out, "more") {
		t.Fatalf("block: out=%q err=%v", out, err)
	}
	text := "a bad word here and another bad word"
	out, n, err := run(t, s, Flag, text, []int{4, 4, 4})
	if err != nil || n != 2 || out != text {
		t.Fatalf("flag: out=%q n=%d err=%v", out, n, err)
	}
}

func TestOverlappingPatterns(t *testing.T) {
	s := mustSet(t, "abc", "bcd", "c")
	out, n, _ := run(t, s, Redact, "xabcdx", []int{1, 1, 1, 1, 1, 1})
	if out != "x****x" || n != 3 {
		t.Fatalf("out=%q n=%d", out, n)
	}
}

func TestMultibyteSplitInsideRune(t *testing.T) {
	s := mustSet(t, "café noir")
	text := "un café noir svp"
	// split in the middle of the 2-byte é
	i := strings.Index(text, "é") + 1
	out, n, _ := run(t, s, Redact, text, []int{i})
	if n != 1 || out != "un ********* svp" {
		t.Fatalf("out=%q n=%d", out, n)
	}
}

func TestCompileErrorsAndDedup(t *testing.T) {
	if _, err := Compile("x", []string{"ok", ""}); err == nil {
		t.Fatal("empty pattern accepted")
	}
	s := mustSet(t, "Foo", "foo", "ＦＯＯ")
	if s.Len() != 1 {
		t.Fatalf("duplicates after normalization not merged: %d", s.Len())
	}
}

// streamedMatches must equal whole-text Scan for any chunking: the core
// property that split patterns are never missed.
func checkSplit(t *testing.T, s *Set, text string, r *rand.Rand) {
	var sizes []int
	for left := len(text); left > 0; {
		n := 1 + r.IntN(8)
		sizes = append(sizes, n)
		left -= n
	}
	want := len(s.Scan(text))
	out, got, _ := run(t, s, Redact, text, sizes)
	if got != want {
		t.Fatalf("streamed %d matches, whole-text %d\ntext=%q sizes=%v", got, want, text, sizes)
	}
	if left := s.Scan(out); len(left) != 0 {
		t.Fatalf("redacted output still contains matches %v: %q", left, out)
	}
}

func TestSplitPatternsRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	pats := []string{"ssn", "credit card", "password", "kill", "home address", "ab", "aba"}
	s := mustSet(t, pats...)
	alphabet := []string{"a", "b", "s", "n", " ", "x", "ssn", "password", "kill ", "credit card", "HOME address", "Ａ"}
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for j := 0; j < 5+r.IntN(30); j++ {
			b.WriteString(alphabet[r.IntN(len(alphabet))])
		}
		checkSplit(t, s, b.String(), r)
	}
}

func FuzzSplitPatterns(f *testing.F) {
	f.Add("the password is ssn credit card", uint64(1))
	f.Add("ＡＢａｂａ", uint64(2))
	f.Add("café noir", uint64(3))
	s, _ := Compile("fuzz", []string{"password", "ssn", "credit card", "aba", "ab", "café noir"})
	f.Fuzz(func(t *testing.T, text string, seed uint64) {
		checkSplit(t, s, text, rand.New(rand.NewPCG(seed, 7)))
	})
}
