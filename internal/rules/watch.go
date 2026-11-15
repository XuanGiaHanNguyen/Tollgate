package rules

import (
	"bytes"
	"context"
	"os"
	"time"
)

// Watch polls path every interval and calls onChange with a newly compiled
// rule set whenever the file's content changes. Compile errors are passed to
// onChange with a nil RuleSet; callers keep serving the previous version.
// The first load happens synchronously before Watch returns, so callers can
// refuse to start without valid rules.
//
// Polling (instead of fsnotify) keeps the standard-library-only rule and
// handles editors that replace files by rename.
func Watch(ctx context.Context, path string, interval time.Duration, o Options, onChange func(*RuleSet, error)) error {
	last, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	rs, err := Compile(string(last), o)
	onChange(rs, err)
	if err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cur, err := os.ReadFile(path)
			if err != nil {
				onChange(nil, err)
				continue
			}
			if bytes.Equal(cur, last) {
				continue
			}
			last = cur
			rs, err := Compile(string(cur), o)
			onChange(rs, err)
		}
	}()
	return nil
}
