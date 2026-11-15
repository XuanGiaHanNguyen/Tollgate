// Package filter catches unsafe phrases in streamed model output. A pattern
// set compiles into one Aho-Corasick automaton over NFKC-normalized,
// lower-cased runes, so look-alike characters (full-width letters,
// ligatures) and case changes do not bypass it. A Stream keeps the
// automaton state across chunks, so a phrase split across a chunk boundary
// is still caught, and holds back the last (longest pattern − 1) runes so
// such a phrase can still be redacted before any of it is released.
package filter

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Action on a match.
type Action int

const (
	Redact Action = iota // replace the matched runes with '*'
	Block                // stop the stream
	Flag                 // count only
)

// ParseAction maps a rule-language action name.
func ParseAction(s string) (Action, bool) {
	switch s {
	case "redact":
		return Redact, true
	case "block":
		return Block, true
	case "flag":
		return Flag, true
	}
	return 0, false
}

func (a Action) String() string { return [...]string{"redact", "block", "flag"}[a] }

// ErrBlocked is returned by Write and Close once a Block-action match ends
// the stream.
var ErrBlocked = errors.New("stream blocked by safety filter")

type edge struct {
	r    rune
	next int32
}

type node struct {
	edges   []edge // sorted by rune; the root uses ascii for runes < 128
	fail    int32
	out     int32 // pattern ending here, -1 if none
	outLink int32 // nearest node on the fail chain with out >= 0, -1 if none
}

// Set is a compiled pattern set. It is immutable and safe to share.
type Set struct {
	Name     string
	nodes    []node
	ascii    [128]int32 // root transitions for ASCII runes (0 = stay at root)
	lens     []int      // rune length of each normalized pattern
	patterns []string   // normalized patterns
	maxLen   int
}

// normalize applies NFKC and lower-cases rune by rune (a 1:1 rune mapping,
// so positions in the lowered text match the normalized text).
func normalize(s string) string { return norm.NFKC.String(s) }

func lower(r rune) rune { return unicode.ToLower(r) }

// Compile builds the automaton. Empty patterns are rejected.
func Compile(name string, patterns []string) (*Set, error) {
	s := &Set{Name: name, nodes: []node{{fail: 0, out: -1, outLink: -1}}}
	seen := map[string]bool{}
	for i, p := range patterns {
		n := strings.Map(lower, normalize(p))
		if n == "" {
			return nil, fmt.Errorf("filter %q: pattern %d is empty", name, i)
		}
		if seen[n] {
			continue // duplicate after normalization
		}
		seen[n] = true
		s.insert(n)
	}
	s.build()
	return s, nil
}

func (s *Set) child(n int32, r rune) int32 {
	es := s.nodes[n].edges
	i := sort.Search(len(es), func(i int) bool { return es[i].r >= r })
	if i < len(es) && es[i].r == r {
		return es[i].next
	}
	return -1
}

func (s *Set) insert(p string) {
	cur := int32(0)
	length := 0
	for _, r := range p {
		length++
		nx := s.child(cur, r)
		if nx < 0 {
			nx = int32(len(s.nodes))
			s.nodes = append(s.nodes, node{out: -1, outLink: -1})
			es := s.nodes[cur].edges
			i := sort.Search(len(es), func(i int) bool { return es[i].r >= r })
			es = append(es, edge{})
			copy(es[i+1:], es[i:])
			es[i] = edge{r, nx}
			s.nodes[cur].edges = es
		}
		cur = nx
	}
	s.nodes[cur].out = int32(len(s.patterns))
	s.patterns = append(s.patterns, p)
	s.lens = append(s.lens, length)
	if length > s.maxLen {
		s.maxLen = length
	}
}

// build computes fail and dictionary-suffix links breadth-first.
func (s *Set) build() {
	queue := make([]int32, 0, len(s.nodes))
	for _, e := range s.nodes[0].edges {
		s.nodes[e.next].fail = 0
		queue = append(queue, e.next)
		if e.r < 128 {
			s.ascii[e.r] = e.next
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, e := range s.nodes[n].edges {
			f := s.nodes[n].fail
			for {
				if t := s.step0(f, e.r); t >= 0 {
					s.nodes[e.next].fail = t
					break
				}
				if f == 0 {
					s.nodes[e.next].fail = 0
					break
				}
				f = s.nodes[f].fail
			}
			fl := s.nodes[e.next].fail
			if s.nodes[fl].out >= 0 {
				s.nodes[e.next].outLink = fl
			} else {
				s.nodes[e.next].outLink = s.nodes[fl].outLink
			}
			queue = append(queue, e.next)
		}
	}
}

// step0 is a direct goto (no fail traversal); -1 if no edge. From the root
// a missing edge means "stay at the root" only in step, not here.
func (s *Set) step0(n int32, r rune) int32 {
	if n == 0 && r < 128 {
		if t := s.ascii[r]; t != 0 {
			return t
		}
		return -1
	}
	return s.child(n, r)
}

// step advances the automaton by one lowered rune.
func (s *Set) step(n int32, r rune) int32 {
	for {
		if t := s.step0(n, r); t >= 0 {
			return t
		}
		if n == 0 {
			return 0
		}
		n = s.nodes[n].fail
	}
}

// MaxLen is the longest pattern in runes (after normalization).
func (s *Set) MaxLen() int { return s.maxLen }

// Len is the number of distinct patterns.
func (s *Set) Len() int { return len(s.patterns) }

// Match is one occurrence: rune offsets [Start, End) in the normalized text.
type Match struct {
	Pattern    int
	Start, End int
}

// Scan finds every match in a whole text (normalized first). Used by tests
// and as the non-streaming reference.
func (s *Set) Scan(text string) []Match {
	var out []Match
	st := int32(0)
	i := 0
	for _, r := range normalize(text) {
		st = s.step(st, lower(r))
		i++
		out = s.collect(st, i, out)
	}
	return out
}

func (s *Set) collect(st int32, end int, out []Match) []Match {
	for n := st; n >= 0; n = s.nodes[n].outLink {
		if p := s.nodes[n].out; p >= 0 {
			out = append(out, Match{Pattern: int(p), Start: end - s.lens[p], End: end})
		}
		if n == 0 {
			break
		}
	}
	return out
}

// Stream filters one response. Not safe for concurrent use.
type Stream struct {
	set     *Set
	action  Action
	state   int32
	nw      *normWriter
	buf     []rune // released lag: normalized runes not yet emitted
	base    int    // absolute rune index of buf[0]
	pos     int    // absolute index of the next rune
	blocked bool

	matches    int
	heldSince  time.Time // when the current holdback started
	LastHold   time.Duration
	OnHoldback func(time.Duration) // observes how long held-back runes waited
}

// NewStream starts filtering one response with the given action.
func (s *Set) NewStream(action Action) *Stream {
	st := &Stream{set: s, action: action}
	st.nw = &normWriter{}
	st.nw.w = norm.NFKC.Writer(&st.nw.sink)
	return st
}

// normWriter collects NFKC output; the norm writer itself holds back an
// incomplete trailing segment until more input or Close.
type normWriter struct {
	w    io.WriteCloser
	sink bytesSink
}

type bytesSink struct{ b []byte }

func (b *bytesSink) Write(p []byte) (int, error) { b.b = append(b.b, p...); return len(p), nil }

// Write feeds a chunk and returns the text that is now safe to release.
func (st *Stream) Write(chunk string) (string, error) {
	if st.blocked {
		return "", ErrBlocked
	}
	st.nw.w.Write([]byte(chunk))
	st.consume(false)
	return st.release(false)
}

// Close flushes everything (normalization tail and holdback).
func (st *Stream) Close() (string, error) {
	if st.blocked {
		return "", ErrBlocked
	}
	st.nw.w.Close()
	st.consume(true)
	return st.release(true)
}

// Matches is the number of matches so far.
func (st *Stream) Matches() int { return st.matches }

// consume runs newly normalized runes through the automaton.
func (st *Stream) consume(final bool) {
	b := st.nw.sink.b
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 && !utf8.FullRune(b) && !final {
			break // incomplete rune; wait for more
		}
		b = b[size:]
		st.buf = append(st.buf, r)
		st.pos++
		st.state = st.set.step(st.state, lower(r))
		for n := st.state; n > 0; n = st.set.nodes[n].outLink {
			if p := st.set.nodes[n].out; p >= 0 {
				st.matches++
				start := st.pos - st.set.lens[p]
				switch st.action {
				case Redact:
					for i := max(start, st.base); i < st.pos; i++ {
						st.buf[i-st.base] = '*'
					}
				case Block:
					st.blocked = true
				}
			}
			if st.set.nodes[n].outLink < 0 {
				break
			}
		}
		if st.blocked {
			break
		}
	}
	st.nw.sink.b = append(st.nw.sink.b[:0], b...)
}

// release emits everything except the holdback tail (or everything when
// final). With Block, the matched text and everything after it are never
// released.
func (st *Stream) release(final bool) (string, error) {
	if st.blocked {
		st.buf = nil
		return "", ErrBlocked
	}
	hold := st.set.maxLen - 1
	if final || hold < 0 {
		hold = 0
	}
	n := len(st.buf) - hold
	if n <= 0 {
		if len(st.buf) > 0 && st.heldSince.IsZero() {
			st.heldSince = time.Now()
		}
		return "", nil
	}
	if !st.heldSince.IsZero() {
		st.LastHold = time.Since(st.heldSince)
		if st.OnHoldback != nil {
			st.OnHoldback(st.LastHold)
		}
		st.heldSince = time.Time{}
	}
	out := string(st.buf[:n])
	st.buf = append(st.buf[:0], st.buf[n:]...)
	st.base += n
	if len(st.buf) > 0 {
		st.heldSince = time.Now()
	}
	return out, nil
}
