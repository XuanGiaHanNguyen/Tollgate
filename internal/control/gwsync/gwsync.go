// Package gwsync keeps a gateway's rules in sync with the control plane:
// it watches the "rules" key from any control node, hands each new
// version to the gateway, and caches the last accepted rules on disk so a
// restarted gateway keeps serving its last good config even when the
// control plane is unreachable.
package gwsync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/client"
)

// Control-plane keys a gateway follows.
const (
	RulesKey   = "rules"
	RolloutKey = "rollout"
)

type rolloutRecord struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Candidate []byte `json:"candidate"`
	Canary    string `json:"canary"`
}

// Apply installs rules at a control-plane version. commitTS is the
// leader's commit wall clock (unix nanos; 0 if unknown, e.g. from cache).
// An error means the rules were rejected; the gateway keeps the old ones.
type Apply func(version uint64, commitTS int64, src []byte) error

type cached struct {
	Version uint64 `json:"version"`
	Src     []byte `json:"src"`
}

// LoadCache applies the cached rules, if any, and returns their version.
func LoadCache(path string, apply Apply) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var c cached
	if json.Unmarshal(b, &c) != nil || apply(c.Version, 0, c.Src) != nil {
		return 0, false
	}
	return c.Version, true
}

func saveCache(path string, version uint64, src []byte) error {
	b, _ := json.Marshal(cached{Version: version, Src: src})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Run watches until ctx ends, always from version 0: the gateway needs the
// current value of both keys (a canary restarted mid-rollout must still be
// able to revert), and config is small. cached reports that rules were
// already served from the cache, so first is closed at once; otherwise
// first is closed after the first rules are applied. self is this
// gateway's name: while a canary rollout names it, it runs the candidate.
func Run(ctx context.Context, c *client.Client, cachePath string, cached bool, self string, apply Apply, log *slog.Logger, first chan<- struct{}) error {
	if cachePath != "" {
		os.MkdirAll(filepath.Dir(cachePath), 0o755)
	}
	signaled := false
	signal := func() {
		if !signaled && first != nil {
			close(first)
			signaled = true
		}
	}
	if cached {
		signal()
	}
	// Changes committed before the watch started are catch-up, not
	// propagation: don't let them time from their original commit.
	started := time.Now().UnixNano()
	var active []byte    // current "rules" value
	var ro rolloutRecord // current rollout record
	var appliedSrc []byte
	appliedSet := false
	err := c.Watch(ctx, 0, func(r *tollgatev1.WatchResponse) {
		var ver uint64
		var ts int64
		touched := false
		for _, ch := range r.Changes {
			switch ch.Key {
			case RulesKey:
				if ch.Deleted {
					log.Warn("rules deleted on the control plane; keeping the last good version", "version", ch.Version)
					continue
				}
				active = ch.Value
			case RolloutKey:
				ro = rolloutRecord{}
				if !ch.Deleted {
					json.Unmarshal(ch.Value, &ro)
				}
			default:
				continue
			}
			touched, ver, ts = true, ch.Version, ch.CommitTsUnixNano
		}
		if !touched {
			return
		}
		desired := active
		if ro.State == "canary" && ro.Canary == self && self != "" {
			desired = ro.Candidate // this gateway is the canary
		}
		if desired == nil || (appliedSet && string(desired) == string(appliedSrc)) {
			return
		}
		if ts < started {
			ts = 0
		}
		if err := apply(ver, ts, desired); err != nil {
			log.Error("rules from control plane rejected; keeping the last good version", "version", ver, "err", err.Error())
			return
		}
		appliedSrc, appliedSet = desired, true
		if cachePath != "" {
			if err := saveCache(cachePath, ver, desired); err != nil {
				log.Warn("could not cache rules", "err", err.Error())
			}
		}
		signal()
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// WaitFirst blocks until first is closed or ctx ends, logging while it waits.
func WaitFirst(ctx context.Context, first <-chan struct{}, log *slog.Logger) error {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-first:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			log.Warn("waiting for rules from the control plane (no cached rules); not serving yet")
		}
	}
}
