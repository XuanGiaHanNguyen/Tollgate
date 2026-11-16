package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// aiResult is one model call as the client saw it.
type aiResult struct {
	at     time.Duration // send time relative to the run start
	status int
	ttft   time.Duration // time to the first content event (0 if none)
	total  time.Duration
	err    error
}

var aiClient = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1000, MaxIdleConns: 0, IdleConnTimeout: time.Minute}}

// aiLoad sends n requests at a constant arrival rate, round-robin over
// urls, and waits for every response to finish. body(i) builds request i.
func aiLoad(urls []string, apiKey string, rate float64, n int, body func(i int) []byte) []aiResult {
	results := make([]aiResult, n)
	var wg sync.WaitGroup
	start := time.Now()
	gap := time.Duration(float64(time.Second) / rate)
	for i := 0; i < n; i++ {
		time.Sleep(time.Until(start.Add(time.Duration(i) * gap)))
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = aiCall(urls[i%len(urls)], apiKey, body(i), time.Since(start))
		}(i)
	}
	wg.Wait()
	return results
}

func aiCall(url, apiKey string, body []byte, at time.Duration) aiResult {
	res := aiResult{at: at}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := aiClient.Do(req)
	if err != nil {
		res.err = err
		return res
	}
	defer resp.Body.Close()
	res.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		res.total = time.Since(start)
		return res
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if res.ttft == 0 && strings.HasPrefix(line, "data: ") && strings.Contains(line, `"content":"`) && !strings.Contains(line, `"content":""`) {
			res.ttft = time.Since(start)
		}
	}
	if err := sc.Err(); err != nil {
		res.err = err
	}
	res.total = time.Since(start)
	return res
}

// quantileDur returns the q-quantile of ds (sorted copy).
func quantileDur(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), ds...)
	for i := 1; i < len(cp); i++ { // insertion sort is fine for a few thousand
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	i := int(q * float64(len(cp)))
	if i >= len(cp) {
		i = len(cp) - 1
	}
	return cp[i]
}

// mockUsage reads a mockllm's billing totals.
type mockUsage struct {
	Total struct {
		Requests         int64 `json:"requests"`
		Completed        int64 `json:"completed"`
		Cancelled        int64 `json:"cancelled"`
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"total"`
	ByUser map[string]struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		Cancelled        int64 `json:"cancelled"`
	} `json:"by_user"`
}

func readMockUsage(url string) (mockUsage, error) {
	var u mockUsage
	err := getJSON(context.Background(), url+"/admin/usage", &u)
	return u, err
}

// microCost prices tokens at the runner's fixed rates: $1 per 1K input,
// $3 per 1K output (the same table the gateways get).
func microCost(in, out int64) int64 { return in*1000 + out*3000 }

const aiPricingYAML = `ai:
  reservation_ttl: %s
  hedge_min_samples: 20
  pricing:
    "*": {input_per_1k_usd: 1.0, output_per_1k_usd: 3.0}
`
