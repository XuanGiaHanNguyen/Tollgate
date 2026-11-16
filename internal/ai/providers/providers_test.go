package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAndParse(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": comment\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\r\n\r\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	s, err := Open(context.Background(), srv.Client(), "p", srv.URL, []byte(`{"model":"m","stream":false}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got["stream"] != true || got["stream_options"] == nil {
		t.Fatalf("request not forced to stream with usage: %v", got)
	}
	var text string
	var usage *Usage
	done := false
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		text += c.Content
		if c.Usage != nil {
			usage = c.Usage
		}
		done = done || c.Done
	}
	if text != "Hello" || usage == nil || usage.PromptTokens != 7 || usage.CompletionTokens != 2 || !done {
		t.Fatalf("text=%q usage=%+v done=%v", text, usage, done)
	}
}

func TestErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	_, err := Open(context.Background(), srv.Client(), "p", srv.URL, []byte(`{}`), nil)
	var es ErrStatus
	if !errors.As(err, &es) || es.Code != 503 {
		t.Fatalf("err = %v", err)
	}
}

func TestEvents(t *testing.T) {
	if string(ContentEvent("m", "x")) != "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"index\":0}],\"model\":\"m\",\"object\":\"chat.completion.chunk\"}\n\n" {
		t.Fatalf("%q", ContentEvent("m", "x"))
	}
}
