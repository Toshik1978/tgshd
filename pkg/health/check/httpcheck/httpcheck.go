// Package httpcheck checks that a web endpoint answers with an expected
// status. One entry judges one condition, http/<name>.
//
// The request can be routed to a fixed address (connect_to) or to an address
// resolved through DNS-over-HTTPS (doh); either way TLS verification, SNI and
// the Host header keep using the URL host. An optional header takes its value
// from an environment variable, and an optional container must be healthy too.
//
// Verdicts: an answer inside the status range is Good; a status outside it, a
// transport failure (DNS, refused, timeout, TLS, too many redirects) or an
// unhealthy container is Bad. A missing or placeholder secret, a failed DoH
// resolution, a failed container lookup or a cancelled run cannot be judged:
// the condition is omitted and CheckResult.Err says why.
package httpcheck

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

const (
	// maxRedirects is the number of redirects followed before giving up.
	maxRedirects = 5
	userAgent    = "tgshd-health"
)

// errTooManyRedirects stops a redirect chain longer than maxRedirects.
var errTooManyRedirects = errors.New("more than 5 redirects")

// Resolver resolves a host name to its addresses, A records first
// (implemented by *doh.Resolver).
type Resolver interface {
	Lookup(ctx context.Context, host string) ([]netip.Addr, error)
}

// Containers reports whether a container is running and healthy.
type Containers interface {
	ContainerHealthy(ctx context.Context, name string) (bool, error)
}

// Dialer opens network connections (implemented by *net.Dialer).
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Options are the collaborators of a Check.
type Options struct {
	// Dialer opens connections; nil selects a plain net.Dialer.
	Dialer Dialer
	// Resolver resolves the URL host; required when the entry sets doh.
	Resolver Resolver
	// Containers judges the entry's container; required when it sets container.
	Containers Containers
	// LookupEnv reads the header secret; nil selects os.LookupEnv.
	LookupEnv func(key string) (string, bool)
	// TLSConfig is cloned for the requests; nil selects the system roots.
	TLSConfig *tls.Config
}

// Check is one configured HTTP endpoint check.
type Check struct {
	key        string
	url        string
	timeout    time.Duration
	status     [2]int
	header     *config.Header
	container  string
	containers Containers
	lookupEnv  func(key string) (string, bool)
	client     *http.Client
}

// outcome is the judgement of one aspect of the condition.
type outcome struct {
	text string
	bad  bool
	// err is set when the aspect could not be judged.
	err error
}

// New builds the check for one http entry of the configuration.
func New(entry config.HTTP, opts Options) (*Check, error) {
	// The url is never repeated in an error: its userinfo or query may be
	// secret, and the start error is pushed to Telegram and email.
	u, err := url.Parse(entry.URL)
	if err != nil {
		return nil, fmt.Errorf("http/%s: url is not a valid URL", entry.Name)
	}

	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("http/%s: url must be an absolute http or https URL", entry.Name)
	}

	if err := checkOptions(&entry, &opts); err != nil {
		return nil, fmt.Errorf("http/%s: %w", entry.Name, err)
	}

	timeout := entry.Timeout.Duration()

	d := &dialer{base: opts.Dialer, host: u.Hostname(), connectTo: entry.ConnectTo, timeout: timeout}
	if entry.DoH != "" {
		d.resolver = opts.Resolver
	}

	if d.base == nil {
		d.base = &net.Dialer{Timeout: timeout}
	}

	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}

	c := &Check{
		key:        "http/" + entry.Name,
		url:        entry.URL,
		timeout:    timeout,
		status:     entry.Status,
		header:     entry.Header,
		container:  entry.Container,
		containers: opts.Containers,
		lookupEnv:  opts.LookupEnv,
	}
	c.client = newClient(d, opts.TLSConfig, c.checkRedirect)

	return c, nil
}

// Key returns the condition key, http/<name>.
func (c *Check) Key() string { return c.key }

// Run judges the endpoint (and its container, when configured) once.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	header, err := c.headerValue()
	if err != nil {
		return health.CheckResult{Err: err}
	}

	return c.result(c.probe(ctx, header), c.checkContainer(ctx))
}

// probe sends the request and judges the answer.
//
// The request gets the entry's timeout but ends before the run's deadline
// (see health.Bound), so an endpoint that hangs past what is left of the run
// is judged Bad too and the run is never abandoned with its reading.
func (c *Check) probe(ctx context.Context, header string) outcome {
	reqCtx, cancel, limit := health.Bound(ctx, c.timeout)
	defer cancel()
	within := withinText(limit, c.timeout)

	trace := &dohTrace{}
	reqCtx = context.WithValue(reqCtx, traceKey{}, trace)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.url, http.NoBody)
	if err != nil {
		return outcome{err: fmt.Errorf("build request: %w", err)}
	}

	req.Header.Set("User-Agent", userAgent)

	if c.header != nil {
		req.Header.Set(c.header.Name, header)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return failure(ctx, trace, err, within)
	}

	_ = resp.Body.Close()

	if lo, hi := c.status[0], c.status[1]; resp.StatusCode < lo || resp.StatusCode > hi {
		return outcome{bad: true, text: fmt.Sprintf("status %d, expected %d-%d", resp.StatusCode, lo, hi)}
	}

	return outcome{text: fmt.Sprintf("status %d", resp.StatusCode)}
}

// failure classifies a failed request: a DoH lookup that failed or did not
// finish, or a run cancelled by the caller, is not a judgement; anything else
// means the endpoint is down.
func failure(ctx context.Context, trace *dohTrace, err error, within string) outcome {
	if dohErr := trace.failed(within); dohErr != nil {
		return outcome{err: dohErr}
	}

	if ctx.Err() != nil {
		return outcome{err: fmt.Errorf("request not completed: %w", ctx.Err())}
	}

	return outcome{bad: true, text: describe(err, within)}
}

// withinText says how long a request had: its timeout, or, when the run's
// deadline cut it shorter, that deadline.
func withinText(limit, timeout time.Duration) string {
	if limit < timeout {
		return "before the run deadline"
	}

	return "within " + timeout.String()
}

func (c *Check) checkContainer(ctx context.Context) outcome {
	if c.container == "" {
		return outcome{}
	}

	ctx, cancel, _ := health.Bound(ctx, c.timeout)
	defer cancel()

	healthy, err := c.containers.ContainerHealthy(ctx, c.container)

	switch {
	case err != nil:
		return outcome{err: fmt.Errorf("container %s: %w", c.container, err)}
	case !healthy:
		return outcome{bad: true, text: fmt.Sprintf("container %s is not healthy", c.container)}
	default:
		return outcome{text: fmt.Sprintf("container %s is healthy", c.container)}
	}
}

// checkRedirect follows at most maxRedirects and keeps the secret header
// away from any hop whose scheme or host differs from the configured URL
// (net/http only strips Authorization and cookies, and only across domains).
func (c *Check) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return errTooManyRedirects
	}

	if first := via[0].URL; c.header != nil &&
		(req.URL.Scheme != first.Scheme || !strings.EqualFold(req.URL.Host, first.Host)) {
		req.Header.Del(c.header.Name)
	}

	return nil
}

// result merges the aspects into the single condition: any Bad aspect makes
// it Bad (with Err for aspects that could not be judged, a partial failure);
// otherwise an unjudged aspect omits the condition; otherwise it is Good.
func (c *Check) result(parts ...outcome) health.CheckResult {
	var (
		bad, good []string
		errs      []error
	)

	for _, p := range parts {
		switch {
		case p.err != nil:
			errs = append(errs, p.err)
		case p.bad:
			bad = append(bad, p.text)
		case p.text != "":
			good = append(good, p.text)
		}
	}

	res := health.CheckResult{Err: errors.Join(errs...)}

	switch {
	case len(bad) > 0:
		res.Readings = []health.Reading{c.reading(health.Bad, bad)}
	case res.Err == nil:
		res.Readings = []health.Reading{c.reading(health.Good, good)}
	}

	return res
}

func (c *Check) reading(v health.Verdict, texts []string) health.Reading {
	return health.Reading{Key: c.key, Verdict: v, Kind: health.State, Text: strings.Join(texts, "; ")}
}

// checkOptions verifies that the collaborators the entry needs are present.
func checkOptions(entry *config.HTTP, opts *Options) error {
	if entry.DoH != "" && opts.Resolver == nil {
		return errors.New("doh is set but no resolver was provided")
	}

	if entry.Container != "" && opts.Containers == nil {
		return errors.New("container is set but no container source was provided")
	}

	if entry.Header != nil && hasControl(entry.Header.Prefix) {
		return errors.New("header prefix contains a control character")
	}

	return nil
}

// newClient returns a client that dials through d, keeps no idle connections
// (every run measures a fresh connection) and vets redirects with redirect.
func newClient(d *dialer, tlsConfig *tls.Config, redirect func(*http.Request, []*http.Request) error) *http.Client {
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		tlsConfig = tlsConfig.Clone()
	}

	return &http.Client{
		Transport: &http.Transport{
			DialContext:       d.DialContext,
			TLSClientConfig:   tlsConfig,
			DisableKeepAlives: true,
		},
		CheckRedirect: redirect,
	}
}

// describe turns a transport error into a short text. It never includes the
// URL (which may carry a query secret) nor any header.
func describe(err error, within string) string {
	if errors.Is(err, errTooManyRedirects) {
		return errTooManyRedirects.Error()
	}

	if ne, ok := errors.AsType[net.Error](err); errors.Is(err, context.DeadlineExceeded) || (ok && ne.Timeout()) {
		return "no answer " + within
	}

	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok {
		return fmt.Sprintf("DNS lookup of %s failed: %s", dnsErr.Name, dnsErr.Err)
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}

	if certErr, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return "TLS: " + certErr.Err.Error()
	}

	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}

	return "request failed: " + err.Error()
}
