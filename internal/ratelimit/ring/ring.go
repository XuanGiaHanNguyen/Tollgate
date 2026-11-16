// Package ring is a consistent hash ring with virtual nodes. It decides which
// gateway owns each rate-limit key. The hash is deterministic across
// processes (FNV-1a with a splitmix64 finalizer), because every gateway must
// compute the same owner for the same key.
package ring

import (
	"fmt"
	"hash/fnv"
	"sort"
)

// DefaultVNodes is the default number of virtual nodes per member.
const DefaultVNodes = 128

// Ring is immutable; build a new one on membership change.
type Ring struct {
	points []uint64 // sorted
	owners []int    // owners[i] is the member index of points[i]
	nodes  []string // members, sorted
	vnodes int
}

// Hash maps a key to the ring. Exported so tools and tests can reason
// about placement.
func Hash(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return mix(h.Sum64())
}

// mix is the splitmix64 finalizer; FNV alone clusters short similar keys.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// New builds a ring. Member order does not matter. vnodes <= 0 means
// DefaultVNodes.
func New(nodes []string, vnodes int) (*Ring, error) {
	if vnodes <= 0 {
		vnodes = DefaultVNodes
	}
	sorted := append([]string(nil), nodes...)
	sort.Strings(sorted)
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return nil, fmt.Errorf("ring: duplicate member %q", sorted[i])
		}
	}
	r := &Ring{nodes: sorted, vnodes: vnodes}
	type pt struct {
		h     uint64
		owner int
	}
	pts := make([]pt, 0, len(sorted)*vnodes)
	for i, n := range sorted {
		for v := 0; v < vnodes; v++ {
			pts = append(pts, pt{Hash(fmt.Sprintf("%s#%d", n, v)), i})
		}
	}
	// Ties (astronomically rare) break by member name so every process
	// builds the same ring.
	sort.Slice(pts, func(a, b int) bool {
		if pts[a].h != pts[b].h {
			return pts[a].h < pts[b].h
		}
		return pts[a].owner < pts[b].owner
	})
	r.points = make([]uint64, len(pts))
	r.owners = make([]int, len(pts))
	for i, p := range pts {
		r.points[i], r.owners[i] = p.h, p.owner
	}
	return r, nil
}

// Owner returns the member that owns key: the first point clockwise from
// the key's hash. It returns "" for an empty ring.
func (r *Ring) Owner(key string) string {
	if len(r.points) == 0 {
		return ""
	}
	return r.nodes[r.owners[r.search(Hash(key))]]
}

// OwnerHash is Owner for a precomputed hash (benchmarks over many keys).
func (r *Ring) OwnerHash(h uint64) string {
	if len(r.points) == 0 {
		return ""
	}
	return r.nodes[r.owners[r.search(h)]]
}

func (r *Ring) search(h uint64) int {
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i] >= h })
	if i == len(r.points) {
		i = 0 // wrap around
	}
	return i
}

// Nodes returns the members in sorted order.
func (r *Ring) Nodes() []string { return append([]string(nil), r.nodes...) }

// Len is the number of members.
func (r *Ring) Len() int { return len(r.nodes) }

// VNodes is the number of virtual nodes per member.
func (r *Ring) VNodes() int { return r.vnodes }
