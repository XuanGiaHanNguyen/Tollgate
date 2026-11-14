package proxy

import (
	"net"
	"net/http"
	"sort"
	"strings"
)

// Router matches requests to routes. It is immutable after NewRouter, so it
// can be shared by all requests without locks and swapped atomically.
type Router struct {
	routes []*Route
}

// NewRouter orders routes so the first match is the most specific:
// longest prefix first, then routes that constrain host, then methods.
func NewRouter(routes []*Route) *Router {
	rs := append([]*Route(nil), routes...)
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if len(a.prefix) != len(b.prefix) {
			return len(a.prefix) > len(b.prefix)
		}
		if (a.host != "") != (b.host != "") {
			return a.host != ""
		}
		return len(a.methods) > 0 && len(b.methods) == 0
	})
	return &Router{routes: rs}
}

// Match returns the most specific route for r, or nil.
func (rt *Router) Match(r *http.Request) *Route {
	host := hostOnly(r.Host)
	for _, route := range rt.routes {
		if route.matches(r.URL.Path, host, r.Method) {
			return route
		}
	}
	return nil
}

func (r *Route) matches(path, host, method string) bool {
	if !prefixMatch(path, r.prefix) {
		return false
	}
	if r.host != "" && !strings.EqualFold(r.host, host) {
		return false
	}
	if len(r.methods) > 0 {
		ok := false
		for _, m := range r.methods {
			if m == method {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// prefixMatch matches on path-segment boundaries: "/api" matches "/api" and
// "/api/x" but not "/apix". A prefix ending in "/" matches anything under it.
func prefixMatch(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if len(path) == len(prefix) || strings.HasSuffix(prefix, "/") {
		return true
	}
	return path[len(prefix)] == '/'
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}
