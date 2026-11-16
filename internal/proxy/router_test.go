package proxy

import (
	"net/http/httptest"
	"testing"
)

func TestRouterMatch(t *testing.T) {
	routes := []*Route{
		{Name: "root", prefix: "/"},
		{Name: "api", prefix: "/api"},
		{Name: "api-ai", prefix: "/api/ai/"},
		{Name: "api-host", prefix: "/api", host: "admin.example.com"},
		{Name: "api-post", prefix: "/api", methods: []string{"POST"}},
	}
	r := NewRouter(routes)
	tests := []struct {
		method, host, path, want string
	}{
		{"GET", "x", "/", "root"},
		{"GET", "x", "/other", "root"},
		{"GET", "x", "/api", "api"},
		{"GET", "x", "/api/users", "api"},
		{"GET", "x", "/apix", "root"}, // segment boundary
		{"GET", "x", "/api/ai/chat", "api-ai"},
		{"GET", "x", "/api/ai", "api"}, // "/api/ai/" needs the trailing slash
		{"GET", "admin.example.com:8080", "/api/x", "api-host"},
		{"GET", "ADMIN.example.com", "/api/x", "api-host"},
		{"POST", "x", "/api/x", "api-post"},
		{"POST", "admin.example.com", "/api/x", "api-host"}, // host beats method
	}
	for _, tc := range tests {
		req := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		got := r.Match(req)
		if got == nil || got.Name != tc.want {
			name := "<nil>"
			if got != nil {
				name = got.Name
			}
			t.Errorf("%s %s%s -> %s, want %s", tc.method, tc.host, tc.path, name, tc.want)
		}
	}
}

func TestRouterNoMatch(t *testing.T) {
	r := NewRouter([]*Route{{Name: "api", prefix: "/api"}})
	if got := r.Match(httptest.NewRequest("GET", "/web", nil)); got != nil {
		t.Fatalf("matched %s", got.Name)
	}
}

func TestRetryBudget(t *testing.T) {
	b := NewRetryBudget(0.2, 2)
	w1, w2 := b.Withdraw(), b.Withdraw()
	if !w1 || !w2 {
		t.Fatal("initial allowance not available")
	}
	if b.Withdraw() {
		t.Fatal("withdrew beyond initial allowance")
	}
	for i := 0; i < 100; i++ {
		b.Deposit()
	}
	n := 0
	for b.Withdraw() {
		n++
	}
	if n != 20 {
		t.Fatalf("100 requests at 20%% allowed %d retries, want 20", n)
	}
	// Cap: a long quiet period cannot bank more than max(initial,100) retries.
	for i := 0; i < 10000; i++ {
		b.Deposit()
	}
	n = 0
	for b.Withdraw() {
		n++
	}
	if n != 100 {
		t.Fatalf("capped balance allowed %d retries, want 100", n)
	}
}

func TestBackoffBounds(t *testing.T) {
	for attempt := 0; attempt < 40; attempt++ {
		for i := 0; i < 50; i++ {
			d := backoff(attempt, 10e6, 200e6)
			if d < 0 || d > 200e6 {
				t.Fatalf("attempt %d backoff %v out of [0,200ms]", attempt, d)
			}
		}
	}
}
