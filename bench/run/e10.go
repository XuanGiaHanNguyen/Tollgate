package main

import (
	"fmt"
	"log"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/ai/filter"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
)

type e10Run struct {
	MBPerSec     float64 `json:"mb_per_sec"`
	ChunkP50Ns   float64 `json:"chunk_p50_ns"`
	ChunkP99Ns   float64 `json:"chunk_p99_ns"`
	Planted      int     `json:"planted"`
	Found        int     `json:"planted_found"`
	SplitPlanted int     `json:"planted_split_across_chunks"`
	SplitFound   int     `json:"split_found"`
	StreamMatch  int     `json:"stream_matches"`
	ScanMatch    int     `json:"whole_text_matches"`
}

type e10Size struct {
	Patterns      int      `json:"patterns"`
	MaxLenRunes   int      `json:"longest_pattern_runes"`
	HoldbackRunes int      `json:"holdback_runes"`
	HoldbackAt50  float64  `json:"holdback_ms_at_50_tokens_per_s"` // derived: (holdback/4 chars per token)/50 tok/s
	Runs          []e10Run `json:"runs"`
	Median        int      `json:"median_run"`
	RecallPct     float64  `json:"split_recall_pct"` // over all runs
}

// e10Words is the vocabulary for patterns and filler text.
func e10Words(r *rand.Rand, n int) []string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	seen := map[string]bool{}
	var out []string
	for len(out) < n {
		l := 3 + r.IntN(7)
		b := make([]byte, l)
		for i := range b {
			b[i] = letters[r.IntN(len(letters))]
		}
		if w := string(b); !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func runE10() (*benchfmt.Result, error) {
	const corpusBytes = 1 << 20
	sizes := []int{100, 1000, 10000}
	res := newResult("E10", 1, map[string]any{
		"pattern_sets": sizes, "corpus_bytes": corpusBytes,
		"patterns":    "two or three random words from a 5,000-word vocabulary (12-30 runes)",
		"chunks":      "uniform random 1-64 bytes, like SSE token deltas",
		"planted":     "~1 pattern occurrence per 2 KB, many straddling chunk boundaries",
		"holdback_ms": "derived: holdback runes / 4 chars per token / 50 tokens per second",
	})
	res.Env.Topology = "single process (filter only)"
	delete(res.Params, "measure_seconds")
	delete(res.Params, "warmup_seconds")

	r := rand.New(rand.NewPCG(10, 10))
	vocab := e10Words(r, 5000)
	var out []e10Size
	for _, n := range sizes {
		pr := rand.New(rand.NewPCG(uint64(n), 1))
		pats := make([]string, n)
		for i := range pats {
			k := 2 + pr.IntN(2)
			ws := make([]string, k)
			for j := range ws {
				ws[j] = vocab[pr.IntN(len(vocab))]
			}
			pats[i] = strings.Join(ws, " ")
		}
		set, err := filter.Compile(fmt.Sprintf("e10-%d", n), pats)
		if err != nil {
			return nil, err
		}
		sz := e10Size{Patterns: set.Len(), MaxLenRunes: set.MaxLen(), HoldbackRunes: set.MaxLen() - 1}
		sz.HoldbackAt50 = round(float64(sz.HoldbackRunes)/4/50*1000, 1)
		var planted, found int
		for rep := 0; rep < *reps; rep++ {
			run := e10Once(set, pats, vocab, corpusBytes, rand.New(rand.NewPCG(uint64(n), uint64(rep+2))))
			sz.Runs = append(sz.Runs, run)
			planted += run.Planted
			found += run.Found
			log.Printf("E10 patterns=%d rep=%d %.1f MB/s chunk p99=%.0fns planted=%d found=%d split=%d/%d stream=%d scan=%d",
				n, rep+1, run.MBPerSec, run.ChunkP99Ns, run.Planted, run.Found, run.SplitFound, run.SplitPlanted, run.StreamMatch, run.ScanMatch)
		}
		_, sz.Median = median(sz.Runs, func(x e10Run) float64 { return x.MBPerSec })
		sz.RecallPct = round(100*float64(found)/float64(planted), 4)
		out = append(out, sz)
	}
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(map[string]any{"sizes": out})
	res.Raw = mustJSON(out)
	return res, nil
}

func e10Once(set *filter.Set, pats, vocab []string, size int, r *rand.Rand) e10Run {
	// Build a corpus of filler words with planted patterns; record each
	// planted occurrence's rune span.
	type span struct{ start, end int }
	var b strings.Builder
	var spans []span
	runes := 0
	for b.Len() < size {
		var w string
		if r.IntN(200) == 0 {
			w = pats[r.IntN(len(pats))]
			n := utf8.RuneCountInString(w)
			spans = append(spans, span{runes, runes + n})
		} else {
			w = vocab[r.IntN(len(vocab))]
		}
		b.WriteString(w)
		b.WriteByte(' ')
		runes += utf8.RuneCountInString(w) + 1
	}
	text := b.String()

	// Chunk it.
	var chunks []string
	var bounds []int // rune offset where each chunk ends
	pos, rpos := 0, 0
	for pos < len(text) {
		n := 1 + r.IntN(64)
		if pos+n > len(text) {
			n = len(text) - pos
		}
		chunks = append(chunks, text[pos:pos+n])
		pos += n
		rpos += utf8.RuneCountInString(text[pos-n : pos])
		bounds = append(bounds, rpos)
	}

	matched := map[span]bool{}
	st := set.NewStream(filter.Flag)
	st.OnMatch = func(m filter.Match) { matched[span{m.Start, m.End}] = true }
	// Pass 1: per-chunk latency and correctness.
	times := make([]float64, 0, len(chunks))
	for _, c := range chunks {
		t0 := time.Now()
		st.Write(c)
		times = append(times, float64(time.Since(t0).Nanoseconds()))
	}
	st.Close()
	// Pass 2: throughput without per-chunk timer overhead.
	tp := set.NewStream(filter.Flag)
	start := time.Now()
	for _, c := range chunks {
		tp.Write(c)
	}
	tp.Close()
	elapsed := time.Since(start)

	run := e10Run{MBPerSec: round(float64(len(text))/elapsed.Seconds()/1e6, 2), Planted: len(spans),
		StreamMatch: st.Matches(), ScanMatch: len(set.Scan(text))}
	sort.Float64s(times)
	run.ChunkP50Ns = times[len(times)/2]
	run.ChunkP99Ns = times[len(times)*99/100]
	for _, s := range spans {
		// a planted span is "split" if a chunk boundary falls strictly inside it
		i := sort.SearchInts(bounds, s.start+1)
		split := i < len(bounds) && bounds[i] < s.end
		if split {
			run.SplitPlanted++
		}
		if matched[s] {
			run.Found++
			if split {
				run.SplitFound++
			}
		}
	}
	return run
}
