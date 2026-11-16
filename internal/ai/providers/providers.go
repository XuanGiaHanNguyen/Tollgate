// Package providers talks to OpenAI-compatible chat APIs: it opens a
// streaming request and parses the SSE event stream into content deltas
// and usage.
package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Usage is what the provider reports it billed.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// Chunk is one parsed SSE event.
type Chunk struct {
	Content string // delta text ("" for role/finish/usage events)
	Usage   *Usage // set on the usage event
	Done    bool   // the [DONE] sentinel
	Raw     []byte // the event's data, for pass-through
}

// Stream reads SSE events from one provider response.
type Stream struct {
	Provider string
	resp     *http.Response
	r        *bufio.Reader
}

// ErrStatus wraps a non-200 provider answer.
type ErrStatus struct{ Code int }

func (e ErrStatus) Error() string { return fmt.Sprintf("provider answered %d", e.Code) }

// Open posts a chat request (forcing stream:true and usage reporting) and
// returns once response headers arrive.
func Open(ctx context.Context, client *http.Client, provider, url string, body []byte, hdr http.Header) (*Stream, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["stream"] = json.RawMessage("true")
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	b, _ := json.Marshal(m)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, ErrStatus{resp.StatusCode}
	}
	return &Stream{Provider: provider, resp: resp, r: bufio.NewReaderSize(resp.Body, 8<<10)}, nil
}

// Close releases the response.
func (s *Stream) Close() error { return s.resp.Body.Close() }

// Next returns the next event. io.EOF means the stream ended (with or
// without [DONE]).
func (s *Stream) Next() (Chunk, error) {
	var data []byte
	for {
		line, err := s.r.ReadSlice('\n')
		if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
			if errors.Is(err, bufio.ErrBufferFull) {
				return Chunk{}, errors.New("SSE line too long")
			}
			if len(data) > 0 {
				return parse(data)
			}
			return Chunk{}, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if len(data) > 0 {
				return parse(data)
			}
			continue
		}
		if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			v = bytes.TrimPrefix(v, []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, v...)
		}
		// other SSE fields (event:, id:, comments) are ignored
	}
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

func parse(data []byte) (Chunk, error) {
	raw := append([]byte(nil), data...)
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return Chunk{Done: true, Raw: raw}, nil
	}
	var w wireChunk
	if err := json.Unmarshal(data, &w); err != nil {
		return Chunk{}, fmt.Errorf("bad SSE event: %w", err)
	}
	c := Chunk{Usage: w.Usage, Raw: raw}
	if len(w.Choices) > 0 {
		c.Content = w.Choices[0].Delta.Content
	}
	return c, nil
}

// ContentEvent builds an SSE data line carrying a content delta, used when
// the gateway rewrites content (safety filter) or flushes held-back text.
func ContentEvent(model, content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}}},
	})
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// ErrorEvent builds an SSE event that ends a stream with an error.
func ErrorEvent(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "tollgate"}})
	return append(append([]byte("data: "), b...), '\n', '\n')
}
