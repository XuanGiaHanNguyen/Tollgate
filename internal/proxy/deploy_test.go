package proxy

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/config"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules"
)

// The shipped deploy configs and rule files must load, compile and only
// reference upstreams and pattern sets that exist.
func TestDeployConfigsValid(t *testing.T) {
	for _, dir := range []string{"../../deploy", "../../deploy/local"} {
		t.Run(dir, func(t *testing.T) {
			cfg, err := config.Load(filepath.Join(dir, "gateway.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			g, err := New(cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			file := cfg.Rules.File
			if cfg.Control != nil {
				file = "rules.tg" // what rules-init pushes to the control plane
			}
			src, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			rs, err := rules.Compile(string(src), rules.Options{Budget: cfg.Rules.Budget})
			if err != nil {
				t.Fatal(err)
			}
			if err := g.CheckRules(rs); err != nil {
				t.Fatal(err)
			}
			if len(rs.Warnings) > 0 {
				t.Errorf("warnings: %v", rs.Warnings)
			}
		})
	}
}
