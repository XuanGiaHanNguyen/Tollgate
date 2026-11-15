// Package env is the runtime shared by the VM and the interpreter: the
// request view rules can read, built-in functions, values, and the decision
// a rule evaluation produces. Keeping it shared guarantees both engines see
// identical inputs and produce comparable outputs.
package env

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Type of a rule value.
type Type uint8

const (
	Invalid Type = iota
	Int
	String
	Bool
)

func (t Type) String() string {
	return [...]string{"invalid", "int", "string", "bool"}[t]
}

// Value is a tagged value. No interfaces, so pushing one never allocates.
type Value struct {
	T Type
	I int64 // Int, and Bool as 0/1
	S string
}

func IntVal(i int64) Value     { return Value{T: Int, I: i} }
func StringVal(s string) Value { return Value{T: String, S: s} }
func BoolVal(b bool) Value {
	if b {
		return Value{T: Bool, I: 1}
	}
	return Value{T: Bool}
}

// Bool reports a Bool value's truth.
func (v Value) Bool() bool { return v.I != 0 }

// Equal compares values of the same type.
func (v Value) Equal(o Value) bool {
	if v.T == String {
		return v.S == o.S
	}
	return v.I == o.I
}

// ---- fields ----

// FieldID identifies a request field.
type FieldID uint8

const (
	FieldUserID FieldID = iota
	FieldUserTier
	FieldClientIP
	FieldRequestMethod
	FieldRequestPath
	FieldRequestHost
	numFields
)

var fieldNames = map[string]FieldID{
	"user.id": FieldUserID, "user.tier": FieldUserTier, "client.ip": FieldClientIP,
	"request.method": FieldRequestMethod, "request.path": FieldRequestPath, "request.host": FieldRequestHost,
}

// LookupField resolves "obj.name". All fields are strings in M3.
func LookupField(obj, name string) (FieldID, Type, bool) {
	id, ok := fieldNames[obj+"."+name]
	return id, String, ok
}

// ---- request ----

// Request is everything a rule can read about one request.
type Request struct {
	Authenticated bool
	UserID        string
	UserTier      string
	ClientIP      string
	Method        string
	Path          string
	Host          string
	Header        http.Header
	RawQuery      string

	// client IP parsed on first use, so many cidr_match calls in one
	// evaluation parse it once
	ipParsed bool
	ipOK     bool
	ip       netip.Addr
}

// Field returns a request field.
func (r *Request) Field(id FieldID) string {
	switch id {
	case FieldUserID:
		return r.UserID
	case FieldUserTier:
		return r.UserTier
	case FieldClientIP:
		return r.ClientIP
	case FieldRequestMethod:
		return r.Method
	case FieldRequestPath:
		return r.Path
	case FieldRequestHost:
		return r.Host
	}
	return ""
}

// ---- builtins ----

// BuiltinID identifies a built-in function.
type BuiltinID uint8

const (
	BuiltinHeader BuiltinID = iota
	BuiltinQuery
	BuiltinCIDRMatch
)

// Builtin describes a built-in's signature.
type Builtin struct {
	ID     BuiltinID
	Name   string
	Params []Type
	Result Type
}

var builtins = map[string]Builtin{
	"header":     {BuiltinHeader, "header", []Type{String}, String},
	"query":      {BuiltinQuery, "query", []Type{String}, String},
	"cidr_match": {BuiltinCIDRMatch, "cidr_match", []Type{String, String}, Bool},
}

// LookupBuiltin resolves a built-in by name.
func LookupBuiltin(name string) (Builtin, bool) {
	b, ok := builtins[name]
	return b, ok
}

// CanonicalHeaderArg rewrites a constant header() argument at compile time so
// the runtime lookup is a direct map access with no allocation.
func CanonicalHeaderArg(s string) string { return http.CanonicalHeaderKey(s) }

// Call runs a built-in. Arguments are already type-checked.
func Call(id BuiltinID, args []Value, r *Request) Value {
	switch id {
	case BuiltinHeader:
		return StringVal(header(r.Header, args[0].S))
	case BuiltinQuery:
		return StringVal(queryValue(r.RawQuery, args[0].S))
	case BuiltinCIDRMatch:
		return BoolVal(r.cidrMatch(args[0].S, args[1].S))
	}
	return Value{}
}

func header(h http.Header, key string) string {
	if v := h[key]; len(v) > 0 { // fast path: key already canonical
		return v[0]
	}
	return h.Get(key)
}

// queryValue finds the first value of key without allocating unless the
// query contains escapes.
func queryValue(raw, key string) string {
	for raw != "" {
		var pair string
		pair, raw, _ = strings.Cut(raw, "&")
		k, v, _ := strings.Cut(pair, "=")
		if strings.ContainsAny(k, "%+") {
			if uk, err := url.QueryUnescape(k); err == nil {
				k = uk
			}
		}
		if k != key {
			continue
		}
		if strings.ContainsAny(v, "%+") {
			if uv, err := url.QueryUnescape(v); err == nil {
				return uv
			}
		}
		return v
	}
	return ""
}

func (r *Request) cidrMatch(ip, cidr string) bool {
	var addr netip.Addr
	if ip == r.ClientIP {
		if !r.ipParsed {
			a, err := netip.ParseAddr(ip)
			r.ip, r.ipOK, r.ipParsed = a, err == nil, true
		}
		if !r.ipOK {
			return false
		}
		addr = r.ip
	} else {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			return false
		}
		addr = a
	}
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false
	}
	return pfx.Contains(addr.Unmap())
}

// ---- decision ----

// Limits and headers per evaluation are capped so a Decision is a fixed-size
// value and evaluation does not allocate.
const (
	MaxLimits  = 8
	MaxHeaders = 8
)

// Errors that make evaluation fail closed.
var (
	ErrBudget      = errors.New("rule evaluation exceeded its instruction budget")
	ErrDivZero     = errors.New("division by zero")
	ErrTooMany     = errors.New("too many limits or headers in one evaluation")
	ErrStack       = errors.New("rule VM stack overflow")
	ErrBadBytecode = errors.New("malformed bytecode")
)

// LimitReq is one limit that applies to this request.
type LimitReq struct {
	ID    int // unique per program; identifies the bucket together with Key
	Count int64
	Per   time.Duration
	Key   string
}

// HeaderSet is one header to add.
type HeaderSet struct{ Name, Value string }

// Decision is the result of evaluating a route's rules.
type Decision struct {
	Deny     int // 0 = allowed; otherwise the HTTP status
	Limits   [MaxLimits]LimitReq
	NLimits  int
	Headers  [MaxHeaders]HeaderSet
	NHeaders int
	Steps    int   // instructions (VM) or nodes (interpreter) executed
	Err      error // non-nil: evaluation failed; callers fail closed
}

// AddLimit appends a limit, reporting false when the cap is reached.
func (d *Decision) AddLimit(l LimitReq) bool {
	if d.NLimits == MaxLimits {
		return false
	}
	d.Limits[d.NLimits] = l
	d.NLimits++
	return true
}

// AddHeader appends a header, reporting false when the cap is reached.
func (d *Decision) AddHeader(h HeaderSet) bool {
	if d.NHeaders == MaxHeaders {
		return false
	}
	d.Headers[d.NHeaders] = h
	d.NHeaders++
	return true
}

// SameOutcome compares the observable parts of two decisions (not Steps),
// used by differential tests.
func (d *Decision) SameOutcome(o *Decision) bool {
	if (d.Err == nil) != (o.Err == nil) || d.Deny != o.Deny || d.NLimits != o.NLimits || d.NHeaders != o.NHeaders {
		return false
	}
	if d.Err != nil {
		return errors.Is(d.Err, o.Err) || d.Err.Error() == o.Err.Error()
	}
	for i := 0; i < d.NLimits; i++ {
		if d.Limits[i] != o.Limits[i] {
			return false
		}
	}
	for i := 0; i < d.NHeaders; i++ {
		if d.Headers[i] != o.Headers[i] {
			return false
		}
	}
	return true
}
