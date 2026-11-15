// Package peer carries rate-limit checks between gateways: a gRPC server
// that applies checks on the owner node, and a client that batches checks
// per peer (flush at MaxBatch checks or FlushInterval, whichever is first).
package peer

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/bucket"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/cost"
)

// Server answers CheckLimits, Reserve and Settle for the keys this node owns.
type Server struct {
	tollgatev1.UnimplementedRateLimitServiceServer
	Store  *bucket.Store
	Budget *cost.Store
}

// Reserve holds an estimated cost on this node.
func (s *Server) Reserve(_ context.Context, r *tollgatev1.ReserveRequest) (*tollgatev1.ReserveResponse, error) {
	ok, bal := s.Budget.Reserve(r.ReservationId, cost.Key{BudgetID: int(r.BudgetId), Value: r.Key},
		r.EstimateMicroUsd, r.AmountMicroUsd, time.Duration(r.PerNs), time.Duration(r.TtlNs))
	return &tollgatev1.ReserveResponse{Ok: ok, BalanceMicroUsd: bal}, nil
}

// Settle charges the real cost for a reservation on this node.
func (s *Server) Settle(_ context.Context, r *tollgatev1.SettleRequest) (*tollgatev1.SettleResponse, error) {
	applied := s.Budget.Settle(r.ReservationId, cost.Key{BudgetID: int(r.BudgetId), Value: r.Key}, r.ActualMicroUsd)
	return &tollgatev1.SettleResponse{Applied: applied}, nil
}

// CheckLimits applies each check to the local store, in order.
func (s *Server) CheckLimits(_ context.Context, req *tollgatev1.CheckLimitsRequest) (*tollgatev1.CheckLimitsResponse, error) {
	out := make([]bool, len(req.Checks))
	for i, c := range req.Checks {
		out[i] = s.Store.AllowAlgo(bucket.Key{LimitID: int(c.LimitId), Value: c.Key},
			c.Count, time.Duration(c.PerNs), AlgoFromProto(c.Algorithm))
	}
	return &tollgatev1.CheckLimitsResponse{Allowed: out}, nil
}

// AlgoFromProto maps the wire enum to the store's algorithm.
func AlgoFromProto(a tollgatev1.Algorithm) bucket.Algorithm {
	if a == tollgatev1.Algorithm_ALGORITHM_SLIDING_WINDOW {
		return bucket.SlidingWindow
	}
	return bucket.TokenBucket
}

// AlgoToProto maps the store's algorithm to the wire enum.
func AlgoToProto(a bucket.Algorithm) tollgatev1.Algorithm {
	if a == bucket.SlidingWindow {
		return tollgatev1.Algorithm_ALGORITHM_SLIDING_WINDOW
	}
	return tollgatev1.Algorithm_ALGORITHM_TOKEN_BUCKET
}

// Config tunes a client.
type Config struct {
	MaxBatch      int           // checks per RPC; 1 disables batching
	FlushInterval time.Duration // longest a check waits for its batch to fill
	Timeout       time.Duration // per check, including batching delay
}

// Hooks receive per-RPC events (metrics). Any may be nil.
type Hooks struct {
	RPC func(peer string, seconds float64, batch int, err error)
}

// ErrTimeout is returned when the peer does not answer in time.
var ErrTimeout = errors.New("peer check timed out")

type result struct {
	allowed bool
	err     error
}

type call struct {
	check *tollgatev1.Check
	done  chan result // buffered 1, so the batcher never blocks on it
}

// Client sends checks to one peer.
type Client struct {
	name  string
	conn  *grpc.ClientConn
	rpc   tollgatev1.RateLimitServiceClient
	cfg   Config
	hooks Hooks
	reqs  chan *call
	stop  chan struct{}
	wg    sync.WaitGroup
}

// Dial connects to a peer lazily (gRPC connects on first use and reconnects
// on its own) and starts the batcher.
func Dial(name, addr string, cfg Config, hooks Hooks) (*Client, error) {
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 500 * time.Microsecond
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Millisecond
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	c := &Client{
		name: name, conn: conn, rpc: tollgatev1.NewRateLimitServiceClient(conn),
		cfg: cfg, hooks: hooks, reqs: make(chan *call, 4096), stop: make(chan struct{}),
	}
	c.wg.Add(1)
	go c.batcher()
	return c, nil
}

// Close stops the batcher and the connection.
func (c *Client) Close() error {
	close(c.stop)
	c.wg.Wait()
	return c.conn.Close()
}

// Name is the peer's member name.
func (c *Client) Name() string { return c.name }

// Reserve forwards a reservation to the peer (not batched: one per model
// call, far rarer than limit checks).
func (c *Client) Reserve(ctx context.Context, r *tollgatev1.ReserveRequest) (*tollgatev1.ReserveResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	return c.rpc.Reserve(ctx, r)
}

// Settle forwards a settlement to the peer.
func (c *Client) Settle(ctx context.Context, r *tollgatev1.SettleRequest) (*tollgatev1.SettleResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	return c.rpc.Settle(ctx, r)
}

// Check asks the peer to apply one check and waits for the answer.
func (c *Client) Check(ctx context.Context, ck *tollgatev1.Check) (bool, error) {
	cl := &call{check: ck, done: make(chan result, 1)}
	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()
	select {
	case c.reqs <- cl:
	case <-timer.C:
		return false, ErrTimeout
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case r := <-cl.done:
		return r.allowed, r.err
	case <-timer.C:
		return false, ErrTimeout
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// batcher groups queued checks and sends each batch in its own goroutine,
// so one slow RPC does not hold up the next batch.
func (c *Client) batcher() {
	defer c.wg.Done()
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	for {
		var first *call
		select {
		case <-c.stop:
			return
		case first = <-c.reqs:
		}
		batch := make([]*call, 1, c.cfg.MaxBatch)
		batch[0] = first
		if c.cfg.MaxBatch > 1 {
			flush.Reset(c.cfg.FlushInterval)
		collect:
			for len(batch) < c.cfg.MaxBatch {
				select {
				case cl := <-c.reqs:
					batch = append(batch, cl)
				case <-flush.C:
					break collect
				case <-c.stop:
					return
				}
			}
			if !flush.Stop() {
				select { // drain a fire that raced with a full batch
				case <-flush.C:
				default:
				}
			}
		}
		go c.send(batch)
	}
}

func (c *Client) send(batch []*call) {
	req := &tollgatev1.CheckLimitsRequest{Checks: make([]*tollgatev1.Check, len(batch))}
	for i, cl := range batch {
		req.Checks[i] = cl.check
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()
	start := time.Now()
	resp, err := c.rpc.CheckLimits(ctx, req)
	if err == nil && len(resp.Allowed) != len(batch) {
		err = errors.New("peer returned a result count that does not match the batch")
	}
	if c.hooks.RPC != nil {
		c.hooks.RPC(c.name, time.Since(start).Seconds(), len(batch), err)
	}
	for i, cl := range batch {
		if err != nil {
			cl.done <- result{err: err}
		} else {
			cl.done <- result{allowed: resp.Allowed[i]}
		}
	}
}
