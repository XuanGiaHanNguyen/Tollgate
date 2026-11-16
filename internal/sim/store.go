package sim

import (
	"fmt"
	"math/rand/v2"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/store"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft"
)

// storeMachine runs the real control-plane state machine under simulation.
type storeMachine struct{ *store.FSM }

func (m storeMachine) Apply(e raft.Entry) { m.FSM.Apply(e) }

// StoreOptions simulates the control-plane config store: proposals are
// put/delete commands on a few keys (rules, members, ...).
func StoreOptions() Options {
	o := DefaultOptions()
	o.NewMachine = func() Machine { return storeMachine{store.New()} }
	keys := []string{"rules", "members", "pricing", "pattern_sets"}
	o.NewProposal = func(r *rand.Rand, n int) []byte {
		c := store.Command{Op: "put", Key: keys[r.IntN(len(keys))], Value: []byte(fmt.Sprintf("v%d", n)), TS: int64(n)}
		switch r.IntN(10) {
		case 0:
			c.Op = "delete"
		case 1, 2: // multi-key txn, sometimes guarded (rollout decisions use these)
			c = store.Command{Op: "txn", TS: int64(n), Ops: []store.Command{
				{Op: "put", Key: "rules", Value: []byte(fmt.Sprintf("t%d", n))},
				{Op: "put", Key: "rollout", Value: []byte(fmt.Sprintf("t%d", n))},
			}}
			if r.IntN(2) == 0 {
				c.If = &store.Cond{Key: "rollout", Version: uint64(r.IntN(50))}
			}
		}
		return c.Encode()
	}
	return o
}
