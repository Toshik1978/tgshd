package httpcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// errNoAddresses reports a DoH answer without any address.
var errNoAddresses = errors.New("no addresses")

// resolveError is a failed DoH resolution. It means the endpoint could not be
// judged, not that it is down.
type resolveError struct {
	host string
	err  error
}

func (e *resolveError) Error() string {
	return fmt.Sprintf("resolve %s via DoH: %v", e.host, e.err)
}

func (e *resolveError) Unwrap() error { return e.err }

// traceKey is the context key of the request's dohTrace.
type traceKey struct{}

// dohTrace records the DoH lookups of one request. net/http dials with a
// context detached from the request's cancellation (values are kept), so a
// request can time out while a lookup is still running; the trace lets the
// check tell that apart from an endpoint that does not answer.
type dohTrace struct {
	mu      sync.Mutex
	host    string
	pending int
	err     error
}

func traceFrom(ctx context.Context) *dohTrace {
	if t, ok := ctx.Value(traceKey{}).(*dohTrace); ok {
		return t
	}

	return nil
}

func (t *dohTrace) start(host string) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.host = host
	t.pending++
}

func (t *dohTrace) finish(err error) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.pending--

	if t.err == nil {
		t.err = err
	}
}

// failed returns the error of a failed lookup, or an error for a lookup that
// has not finished in time (within says when: "within 15s"), or nil.
func (t *dohTrace) failed(within string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch {
	case t.err != nil:
		return t.err
	case t.pending > 0:
		return &resolveError{host: t.host, err: errors.New("no answer " + within)}
	default:
		return nil
	}
}

// dialer routes connections for one entry. The TLS layer and the Host header
// still use the URL host, so only the TCP destination changes.
type dialer struct {
	base Dialer
	// host is the URL host name connectTo applies to.
	host string
	// connectTo is the address that replaces host (port optional).
	connectTo string
	// resolver, when set, resolves every dialed host name via DoH.
	resolver Resolver
	// timeout bounds each dial including its DoH lookup, since net/http
	// does not pass the request deadline to the dial.
	timeout time.Duration
}

// DialContext dials address after applying connect_to or DoH resolution.
func (d *dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}

	if d.timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}

	switch {
	case d.connectTo != "" && strings.EqualFold(host, d.host):
		address = overrideAddress(d.connectTo, port)
	case d.resolver != nil:
		ip, resolveErr := d.resolve(ctx, host)
		if resolveErr != nil {
			return nil, resolveErr
		}

		address = net.JoinHostPort(ip, port)
	}

	conn, err := d.base.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}

	return conn, nil
}

// resolve returns the first DoH address of host and records the lookup in
// the request's trace.
func (d *dialer) resolve(ctx context.Context, host string) (string, error) {
	trace := traceFrom(ctx)
	trace.start(host)

	addrs, err := d.resolver.Lookup(ctx, host)
	if err == nil && len(addrs) == 0 {
		err = errNoAddresses
	}

	if err != nil {
		rerr := &resolveError{host: host, err: err}
		trace.finish(rerr)

		return "", rerr
	}

	trace.finish(nil)

	return addrs[0].String(), nil
}

// overrideAddress returns connectTo with the dialed port unless connectTo
// carries its own port. IPv6 literals may be given with or without brackets.
func overrideAddress(connectTo, port string) string {
	if host, p, err := net.SplitHostPort(connectTo); err == nil {
		return net.JoinHostPort(host, p)
	}

	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(connectTo, "["), "]"), port)
}
