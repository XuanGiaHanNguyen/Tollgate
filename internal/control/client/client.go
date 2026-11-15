// Package client talks to the control plane: writes and linearizable reads
// follow leader redirects and retry through elections; watches reconnect
// to any node and resume from the last version seen.
package client

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
)

// Client is safe for concurrent use.
type Client struct {
	endpoints []string
	mu        sync.Mutex
	conns     map[string]*grpc.ClientConn
	leader    string
}

// New creates a client for the given control-plane endpoints.
func New(endpoints []string) *Client {
	return &Client{endpoints: endpoints, conns: map[string]*grpc.ClientConn{}}
}

// Close closes connections.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cc := range c.conns {
		cc.Close()
	}
}

func (c *Client) svc(addr string) (tollgatev1.ControlServiceClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cc, ok := c.conns[addr]
	if !ok {
		var err error
		cc, err = grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		c.conns[addr] = cc
	}
	return tollgatev1.NewControlServiceClient(cc), nil
}

// order returns endpoints to try: the known leader first.
func (c *Client) order() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.endpoints)+1)
	if c.leader != "" {
		out = append(out, c.leader)
	}
	for _, e := range c.endpoints {
		if e != c.leader {
			out = append(out, e)
		}
	}
	return out
}

func (c *Client) setLeader(addr string) {
	c.mu.Lock()
	c.leader = addr
	c.mu.Unlock()
}

func retryable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	return false
}

// leaderCall runs f against the leader, following redirects and retrying
// through elections until ctx ends. f reports (notLeader, leaderAddr, err).
func (c *Client) leaderCall(ctx context.Context, f func(ctx context.Context, s tollgatev1.ControlServiceClient) (bool, string, error)) error {
	backoff := 20 * time.Millisecond
	for {
		var lastErr error
		for _, addr := range c.order() {
			s, err := c.svc(addr)
			if err != nil {
				lastErr = err
				continue
			}
			actx, cancel := context.WithTimeout(ctx, 3*time.Second)
			notLeader, hint, err := f(actx, s)
			cancel()
			switch {
			case err == nil && !notLeader:
				c.setLeader(addr)
				return nil
			case err == nil && notLeader:
				if hint != "" && hint != addr {
					c.setLeader(hint)
				}
				lastErr = errors.New("no leader yet")
			case retryable(err):
				lastErr = err
			default:
				return err // e.g. invalid rules: not worth retrying
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if notLeader && hint != "" {
				break // retry the loop with the hinted leader first
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), lastErr)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 500*time.Millisecond)
	}
}

// Put writes key and returns the committed version.
func (c *Client) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	var v uint64
	err := c.leaderCall(ctx, func(ctx context.Context, s tollgatev1.ControlServiceClient) (bool, string, error) {
		r, err := s.Put(ctx, &tollgatev1.PutRequest{Key: key, Value: value})
		if err != nil {
			return false, "", err
		}
		v = r.Version
		return r.NotLeader, r.LeaderAddr, nil
	})
	return v, err
}

// Get is a linearizable read.
func (c *Client) Get(ctx context.Context, key string) (*tollgatev1.GetResponse, error) {
	var out *tollgatev1.GetResponse
	err := c.leaderCall(ctx, func(ctx context.Context, s tollgatev1.ControlServiceClient) (bool, string, error) {
		r, err := s.Get(ctx, &tollgatev1.GetRequest{Key: key})
		if err != nil {
			return false, "", err
		}
		out = r
		return r.NotLeader, r.LeaderAddr, nil
	})
	return out, err
}

// Status asks one endpoint for its status.
func (c *Client) Status(ctx context.Context, addr string) (*tollgatev1.StatusResponse, error) {
	s, err := c.svc(addr)
	if err != nil {
		return nil, err
	}
	return s.Status(ctx, &tollgatev1.StatusRequest{})
}

// Watch streams changes after from to fn until ctx ends, reconnecting to
// any endpoint and resuming from the last version fn saw.
func (c *Client) Watch(ctx context.Context, from uint64, fn func(*tollgatev1.WatchResponse)) error {
	backoff := 50 * time.Millisecond
	i := 0
	for ctx.Err() == nil {
		addr := c.endpoints[i%len(c.endpoints)]
		i++
		s, err := c.svc(addr)
		if err == nil {
			var stream tollgatev1.ControlService_WatchClient
			stream, err = s.Watch(ctx, &tollgatev1.WatchRequest{FromVersion: from})
			for err == nil {
				var r *tollgatev1.WatchResponse
				r, err = stream.Recv()
				if err != nil {
					break
				}
				fn(r)
				if r.Version > from {
					from = r.Version
				}
				backoff = 50 * time.Millisecond
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Second)
	}
	return ctx.Err()
}
