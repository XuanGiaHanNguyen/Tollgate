package control

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/store"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules"
)

// ---- Raft message <-> protobuf ----

func toProto(m raft.Message) *tollgatev1.RaftMessage {
	pm := &tollgatev1.RaftMessage{Type: int32(m.Type), From: m.From, To: m.To, Term: m.Term, Index: m.Index,
		LogTerm: m.LogTerm, Commit: m.Commit, Reject: m.Reject, RejectHint: m.RejectHint, Context: m.Context}
	for _, e := range m.Entries {
		pm.Entries = append(pm.Entries, &tollgatev1.RaftEntry{Term: e.Term, Index: e.Index, Data: e.Data, Noop: e.Data == nil})
	}
	if m.Snapshot != nil {
		pm.Snapshot = &tollgatev1.RaftSnapshot{Index: m.Snapshot.Index, Term: m.Snapshot.Term, Data: m.Snapshot.Data}
	}
	return pm
}

func fromProto(pm *tollgatev1.RaftMessage) raft.Message {
	m := raft.Message{Type: raft.MsgType(pm.Type), From: pm.From, To: pm.To, Term: pm.Term, Index: pm.Index,
		LogTerm: pm.LogTerm, Commit: pm.Commit, Reject: pm.Reject, RejectHint: pm.RejectHint, Context: pm.Context}
	for _, e := range pm.Entries {
		re := raft.Entry{Term: e.Term, Index: e.Index}
		if !e.Noop {
			re.Data = e.Data
			if re.Data == nil {
				re.Data = []byte{}
			}
		}
		m.Entries = append(m.Entries, re)
	}
	if pm.Snapshot != nil {
		m.Snapshot = &raft.Snapshot{Index: pm.Snapshot.Index, Term: pm.Snapshot.Term, Data: pm.Snapshot.Data}
	}
	return m
}

// ---- transport ----

// GRPCTransport sends Raft messages to peers through one bounded queue and
// sender goroutine per peer. A full queue drops messages (Raft retries).
type GRPCTransport struct {
	peers map[uint64]chan raft.Message
	conns []*grpc.ClientConn
	stop  chan struct{}
}

// NewGRPCTransport dials every peer (lazily, gRPC reconnects on its own).
func NewGRPCTransport(self uint64, raftAddrs map[uint64]string) (*GRPCTransport, error) {
	t := &GRPCTransport{peers: map[uint64]chan raft.Message{}, stop: make(chan struct{})}
	for id, addr := range raftAddrs {
		if id == self {
			continue
		}
		// gRPC's default reconnect backoff grows to minutes; a restarted
		// peer must hear heartbeats within an election timeout.
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithConnectParams(grpc.ConnectParams{MinConnectTimeout: time.Second,
				Backoff: backoff.Config{BaseDelay: 50 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: time.Second}}))
		if err != nil {
			t.Close()
			return nil, err
		}
		t.conns = append(t.conns, conn)
		ch := make(chan raft.Message, 4096)
		t.peers[id] = ch
		go t.sender(tollgatev1.NewRaftServiceClient(conn), ch)
	}
	return t, nil
}

func (t *GRPCTransport) sender(c tollgatev1.RaftServiceClient, ch chan raft.Message) {
	for {
		var first raft.Message
		select {
		case <-t.stop:
			return
		case first = <-ch:
		}
		batch := &tollgatev1.SendRequest{Messages: []*tollgatev1.RaftMessage{toProto(first)}}
	drain:
		for len(batch.Messages) < 64 {
			select {
			case m := <-ch:
				batch.Messages = append(batch.Messages, toProto(m))
			default:
				break drain
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = c.Send(ctx, batch) // lost messages are recovered by Raft
		cancel()
	}
}

// Send queues messages without blocking.
func (t *GRPCTransport) Send(msgs []raft.Message) {
	for _, m := range msgs {
		if ch := t.peers[m.To]; ch != nil {
			select {
			case ch <- m:
			default: // queue full: drop
			}
		}
	}
}

// Close stops senders and connections.
func (t *GRPCTransport) Close() {
	select {
	case <-t.stop:
	default:
		close(t.stop)
	}
	for _, c := range t.conns {
		c.Close()
	}
}

// ---- gRPC services ----

// RaftService receives Raft messages from peers.
type RaftService struct {
	tollgatev1.UnimplementedRaftServiceServer
	S *Server
}

// Send hands each message to the node.
func (r *RaftService) Send(ctx context.Context, req *tollgatev1.SendRequest) (*tollgatev1.SendResponse, error) {
	for _, m := range req.Messages {
		if err := r.S.Receive(ctx, fromProto(m)); err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
	}
	return &tollgatev1.SendResponse{}, nil
}

// API serves the client-facing ControlService.
type API struct {
	tollgatev1.UnimplementedControlServiceServer
	S           *Server
	ClientAddrs map[uint64]string // node ID -> client API address, for redirects
}

func (a *API) leaderAddr() (string, bool) {
	st, _ := a.S.Status()
	return a.ClientAddrs[st.Lead], st.State == raft.Leader
}

// ValidateRules compiles rule source; writes to the "rules" key must pass.
func ValidateRules(src []byte) error {
	_, err := rules.Compile(string(src), rules.Options{})
	return err
}

// Put writes a key through Raft (leader only).
func (a *API) Put(ctx context.Context, req *tollgatev1.PutRequest) (*tollgatev1.PutResponse, error) {
	if req.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}
	if addr, leader := a.leaderAddr(); !leader {
		return &tollgatev1.PutResponse{NotLeader: true, LeaderAddr: addr}, nil
	}
	if req.Key == "rules" {
		if err := ValidateRules(req.Value); err != nil {
			return nil, status.Error(codes.InvalidArgument, "rules rejected:\n"+err.Error())
		}
	}
	v, err := a.S.Propose(ctx, store.Command{Op: "put", Key: req.Key, Value: req.Value})
	return &tollgatev1.PutResponse{Version: v}, a.rpcErr(err)
}

// Delete removes a key through Raft (leader only).
func (a *API) Delete(ctx context.Context, req *tollgatev1.DeleteRequest) (*tollgatev1.DeleteResponse, error) {
	if addr, leader := a.leaderAddr(); !leader {
		return &tollgatev1.DeleteResponse{NotLeader: true, LeaderAddr: addr}, nil
	}
	v, err := a.S.Propose(ctx, store.Command{Op: "delete", Key: req.Key})
	return &tollgatev1.DeleteResponse{Version: v}, a.rpcErr(err)
}

// Get is a linearizable read (leader only).
func (a *API) Get(ctx context.Context, req *tollgatev1.GetRequest) (*tollgatev1.GetResponse, error) {
	it, ok, err := a.S.LinearizableGet(ctx, req.Key)
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, ErrNotLeader) {
		addr, _ := a.leaderAddr()
		return &tollgatev1.GetResponse{NotLeader: true, LeaderAddr: addr}, nil
	}
	if err != nil {
		return nil, a.rpcErr(err)
	}
	return &tollgatev1.GetResponse{Found: ok, Value: it.Value, Version: it.Version}, nil
}

// Watch streams changes from this node's applied state.
func (a *API) Watch(req *tollgatev1.WatchRequest, stream tollgatev1.ControlService_WatchServer) error {
	err := a.S.Watch(stream.Context(), req.FromVersion, func(full bool, changes []store.Change, version uint64) error {
		resp := &tollgatev1.WatchResponse{Full: full, Version: version}
		for _, c := range changes {
			resp.Changes = append(resp.Changes, &tollgatev1.Change{Version: c.Version, Key: c.Key, Value: c.Value, Deleted: c.Deleted, CommitTsUnixNano: c.TS})
		}
		return stream.Send(resp)
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return a.rpcErr(err)
}

// Status reports this node.
func (a *API) Status(context.Context, *tollgatev1.StatusRequest) (*tollgatev1.StatusResponse, error) {
	st, ver := a.S.Status()
	return &tollgatev1.StatusResponse{Id: st.ID, State: st.State.String(), Term: st.Term, Leader: st.Lead,
		LeaderAddr: a.ClientAddrs[st.Lead], Commit: st.Commit, Applied: st.Applied, Version: ver,
		LastIndex: st.LastIndex, SnapshotIndex: st.SnapIndex}, nil
}

func (a *API) rpcErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, raft.ErrNotLeader), errors.Is(err, ErrDropped), errors.Is(err, ErrStopped):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
