package rules

import (
	"bytes"
	"context"
	"errors"
	"os"
	"time"
)

// ErrEmptyRules rejects a rules file with no content. An editor that
// truncates and rewrites a file can be read in between; loading that empty
// file would silently drop all policy (fail open).
var ErrEmptyRules = errors.New("rules file is empty; refusing to drop all policy (write a comment-only file to clear rules on purpose)")

// Watch polls path every interval and calls onChange with a newly compiled
// rule set whenever the file's content changes. Compile errors are passed to
// onChange with a nil RuleSet; callers keep serving the previous version.
// The first load happens synchronously before Watch returns, so callers can
// refuse to start without valid rules.
//
// A change is compiled only once the content is identical on two
// consecutive polls, so a file caught mid-write is never loaded. That adds
// one interval of reload delay. Polling (instead of fsnotify) keeps the
// standard-library-only rule and handles editors that replace files by
// rename.
func Watch(ctx context.Context, path string, interval time.Duration, o Options, onChange func(*RuleSet, error)) error {
	last, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	rs, err := compileFile(last, o)
	onChange(rs, err)
	if err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		var pending []byte // changed content waiting for one stable poll
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cur, err := os.ReadFile(path)
			if err != nil {
				onChange(nil, err)
				pending = nil
				continue
			}
			if bytes.Equal(cur, last) {
				pending = nil
				continue
			}
			if pending == nil || !bytes.Equal(cur, pending) {
				pending = cur // first sighting: wait for it to settle
				continue
			}
			last, pending = cur, nil
			rs, err := compileFile(cur, o)
			onChange(rs, err)
		}
	}()
	return nil
}

func compileFile(src []byte, o Options) (*RuleSet, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return nil, ErrEmptyRules
	}
	return Compile(string(src), o)
}
