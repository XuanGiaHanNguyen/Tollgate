package rules

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

func TestWatchReloadsAndKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.tg")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`route "/*" { deny 401 }`)

	var mu sync.Mutex
	var current *RuleSet
	var errs int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Watch(ctx, path, 10*time.Millisecond, Options{}, func(rs *RuleSet, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs++
			return
		}
		current = rs
	})
	if err != nil {
		t.Fatal(err)
	}
	denyOf := func() int {
		mu.Lock()
		defer mu.Unlock()
		r := current.Match("/x")
		if r == nil {
			return -1 // no policy at all: the bug this guards against
		}
		var d env.Decision
		current.Eval(r, VM, &env.Request{}, &d)
		return d.Deny
	}
	if denyOf() != 401 {
		t.Fatal("initial rules not loaded")
	}

	write(`route "/*" { deny 403 }`)
	waitFor(t, func() bool { return denyOf() == 403 })

	write(`route "/*" { deny }`) // broken: keep serving 403
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return errs > 0 })
	if denyOf() != 403 {
		t.Fatal("broken file replaced the last good rules")
	}
}

func TestWatchRefusesInvalidStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.tg")
	os.WriteFile(path, []byte(`route "/*" {`), 0o644)
	if err := Watch(context.Background(), path, time.Second, Options{}, func(*RuleSet, error) {}); err == nil {
		t.Fatal("started with invalid rules")
	}
	if err := Watch(context.Background(), path+".missing", time.Second, Options{}, func(*RuleSet, error) {}); err == nil {
		t.Fatal("started without a file")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWatchIgnoresEmptyAndPartialWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.tg")
	os.WriteFile(path, []byte(`route "/*" { deny 401 }`), 0o644)
	var mu sync.Mutex
	var loads int
	var lastErr error
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Watch(ctx, path, 10*time.Millisecond, Options{}, func(rs *RuleSet, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			lastErr = err
			return
		}
		loads++
	}); err != nil {
		t.Fatal(err)
	}
	// A truncated file that stays empty is refused, never loaded.
	os.WriteFile(path, nil, 0o644)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return lastErr != nil })
	mu.Lock()
	if !errors.Is(lastErr, ErrEmptyRules) || loads != 1 {
		t.Fatalf("err=%v loads=%d", lastErr, loads)
	}
	mu.Unlock()
	// Starting with an empty file is refused too.
	empty := filepath.Join(t.TempDir(), "empty.tg")
	os.WriteFile(empty, []byte("  \n"), 0o644)
	if err := Watch(context.Background(), empty, time.Second, Options{}, func(*RuleSet, error) {}); !errors.Is(err, ErrEmptyRules) {
		t.Fatalf("err = %v", err)
	}
}
