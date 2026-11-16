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

// RulesKey is the control-plane key holding rule source.
const RulesKey = "rules"

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

// Run watches until ctx ends. from is the version already applied (from
// the cache), so the watch resumes after it. first is closed after the
// first rules are applied from the control plane (or immediately if from>0).
func Run(ctx context.Context, c *client.Client, cachePath string, from uint64, apply Apply, log *slog.Logger, first chan<- struct{}) error {
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
	if from > 0 {
		signal()
	}
	// Changes committed before the watch started are catch-up, not
	// propagation: don't let them time from their original commit.
	started := time.Now().UnixNano()
	err := c.Watch(ctx, from, func(r *tollgatev1.WatchResponse) {
		for _, ch := range r.Changes {
			if ch.Key != RulesKey {
				continue
			}
			if ch.Deleted {
				log.Warn("rules deleted on the control plane; keeping the last good version", "version", ch.Version)
				continue
			}
			ts := ch.CommitTsUnixNano
			if ts < started {
				ts = 0
			}
			if err := apply(ch.Version, ts, ch.Value); err != nil {
				log.Error("rules from control plane rejected; keeping the last good version", "version", ch.Version, "err", err.Error())
				continue
			}
			if cachePath != "" {
				if err := saveCache(cachePath, ch.Version, ch.Value); err != nil {
					log.Warn("could not cache rules", "err", err.Error())
				}
			}
			signal()
		}
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
