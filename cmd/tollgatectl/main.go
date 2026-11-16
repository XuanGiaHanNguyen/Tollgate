// Command tollgatectl administers the control plane: push and read rules,
// read and write keys, show cluster state, follow changes.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/client"
)

const usage = `usage: tollgatectl [-endpoints host:port,...] <command>

commands:
  status                      show every control node (role, term, leader, versions)
  rules push <file>           validate and commit a rule file
  rules check <file>          compile a rule file locally, without committing
  rules get                   print the committed rules
  put <key> <value|@file>     write a key
  get <key>                   linearizable read
  watch [from-version]        stream changes
`

func main() {
	eps := flag.String("endpoints", envOr("TOLLGATE_CONTROL", "127.0.0.1:7201,127.0.0.1:7202,127.0.0.1:7203"), "control-plane client API endpoints")
	timeout := flag.Duration("timeout", 10*time.Second, "per-command timeout (not for watch)")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	endpoints := strings.Split(*eps, ",")
	c := client.New(endpoints)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := run(ctx, c, endpoints, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func run(ctx context.Context, c *client.Client, endpoints, args []string) error {
	switch {
	case args[0] == "status":
		fmt.Printf("%-22s %-10s %-6s %-7s %-8s %-8s %-8s\n", "ENDPOINT", "STATE", "TERM", "LEADER", "COMMIT", "APPLIED", "VERSION")
		for _, ep := range endpoints {
			st, err := c.Status(ctx, ep)
			if err != nil {
				fmt.Printf("%-22s unreachable: %v\n", ep, err)
				continue
			}
			fmt.Printf("%-22s %-10s %-6d %-7d %-8d %-8d %-8d\n", ep, st.State, st.Term, st.Leader, st.Commit, st.Applied, st.Version)
		}
		return nil
	case len(args) == 3 && args[0] == "rules" && args[1] == "push":
		src, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		v, err := c.Put(ctx, "rules", src)
		if err != nil {
			return err
		}
		fmt.Printf("rules committed at version %d\n", v)
		return nil
	case len(args) == 3 && args[0] == "rules" && args[1] == "check":
		src, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		if err := control.ValidateRules(src); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	case len(args) == 2 && args[0] == "rules" && args[1] == "get":
		r, err := c.Get(ctx, "rules")
		if err != nil {
			return err
		}
		if !r.Found {
			return fmt.Errorf("no rules committed")
		}
		fmt.Printf("# version %d\n%s", r.Version, r.Value)
		return nil
	case len(args) == 3 && args[0] == "put":
		val := []byte(args[2])
		if strings.HasPrefix(args[2], "@") {
			var err error
			if val, err = os.ReadFile(args[2][1:]); err != nil {
				return err
			}
		}
		v, err := c.Put(ctx, args[1], val)
		if err != nil {
			return err
		}
		fmt.Printf("%s committed at version %d\n", args[1], v)
		return nil
	case len(args) == 2 && args[0] == "get":
		r, err := c.Get(ctx, args[1])
		if err != nil {
			return err
		}
		if !r.Found {
			return fmt.Errorf("%s not found", args[1])
		}
		fmt.Printf("%s\n", r.Value)
		return nil
	case args[0] == "watch":
		var from uint64
		if len(args) == 2 {
			fmt.Sscan(args[1], &from)
		}
		return c.Watch(context.Background(), from, func(r *tollgatev1.WatchResponse) {
			if r.Full {
				fmt.Printf("-- full sync at version %d\n", r.Version)
			}
			for _, ch := range r.Changes {
				op := "put"
				if ch.Deleted {
					op = "delete"
				}
				fmt.Printf("v%d %s %s (%d bytes)\n", ch.Version, op, ch.Key, len(ch.Value))
			}
		})
	}
	return fmt.Errorf("unknown command %q\n%s", strings.Join(args, " "), usage)
}
