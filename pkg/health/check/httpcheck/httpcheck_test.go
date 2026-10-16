package httpcheck_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/doh"
	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/httpcheck"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

// The production DoH resolver must satisfy the check's resolver interface.
var _ httpcheck.Resolver = (*doh.Resolver)(nil)

const (
	appHost  = "app.example.org"
	key      = "http/web-app"
	secret   = "s3cr3t-T0ken-Value"
	tokenEnv = "APP_TOKEN"
)

// fakeDialer sends every connection to target (or to byPort[port]) and
// records the addresses it was asked for. With block set it waits for the
// dial context to end and then closes done.
type fakeDialer struct {
	mu     sync.Mutex
	target string
	byPort map[string]string
	err    error
	block  bool
	done   chan struct{}
	asked  []string
}

func (d *fakeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.asked = append(d.asked, address)
	d.mu.Unlock()

	if d.block {
		<-ctx.Done()
		close(d.done)

		return nil, ctx.Err()
	}

	if d.err != nil {
		return nil, d.err
	}

	target := d.target
	if _, port, err := net.SplitHostPort(address); err == nil && d.byPort[port] != "" {
		target = d.byPort[port]
	}

	var nd net.Dialer

	return nd.DialContext(ctx, network, target)
}

func (d *fakeDialer) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]string(nil), d.asked...)
}

type fakeResolver struct {
	addrs []netip.Addr
	err   error
	hosts []string
}

func (r *fakeResolver) Lookup(_ context.Context, host string) ([]netip.Addr, error) {
	r.hosts = append(r.hosts, host)

	return r.addrs, r.err
}

// blockingResolver waits for the lookup context to end, then closes done.
type blockingResolver struct {
	done chan struct{}
}

func (r *blockingResolver) Lookup(ctx context.Context, _ string) ([]netip.Addr, error) {
	<-ctx.Done()
	close(r.done)

	return nil, ctx.Err()
}

type fakeContainers struct {
	healthy  bool
	err      error
	names    []string
	deadline bool
}

func (c *fakeContainers) ContainerHealthy(ctx context.Context, name string) (bool, error) {
	c.names = append(c.names, name)
	_, c.deadline = ctx.Deadline()

	return c.healthy, c.err
}

// recorder captures what the server saw.
type recorder struct {
	mu     sync.Mutex
	hosts  []string
	auth   []string
	sni    []string
	keys   map[string][]string // X-Api-Key values by Host
	called int
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.called++
	r.hosts = append(r.hosts, req.Host)
	r.auth = append(r.auth, req.Header.Get("Authorization"))

	if r.keys == nil {
		r.keys = make(map[string][]string)
	}

	r.keys[req.Host] = append(r.keys[req.Host], req.Header.Get("X-Api-Key"))
}

// seen returns copies of the Host headers, Authorization headers and SNI
// names recorded so far.
func (r *recorder) seen() (hosts, auth, sni []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.hosts), slices.Clone(r.auth), slices.Clone(r.sni)
}

func (r *recorder) apiKeys(host string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.keys[host])
}

func (r *recorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.called
}

func statusHandler(rec *recorder, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)
		w.WriteHeader(status)
	})
}

// redirectHandler redirects /hop/N to /hop/N-1 and answers 200 at /hop/0.
func redirectHandler(rec *recorder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)

		n, err := strconv.Atoi(strings.TrimPrefix(req.URL.Path, "/hop/"))
		if err != nil || n <= 0 {
			w.WriteHeader(http.StatusOK)

			return
		}

		http.Redirect(w, req, "/hop/"+strconv.Itoa(n-1), http.StatusFound)
	})
}

// selfSigned returns a certificate for host and a pool that trusts it.
func selfSigned(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, pool
}

// newTLSServer starts a TLS server presenting a certificate for certHost and
// returns it with a client TLS config trusting that certificate.
func newTLSServer(t *testing.T, certHost string, rec *recorder, h http.Handler) (*httptest.Server, *tls.Config) {
	t.Helper()

	cert, pool := selfSigned(t, certHost)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			rec.mu.Lock()
			rec.sni = append(rec.sni, hello.ServerName)
			rec.mu.Unlock()

			return &cert, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func newHTTPServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return srv
}

func entry(rawURL string) config.HTTP {
	return config.HTTP{
		Name:    "web-app",
		URL:     rawURL,
		Timeout: config.Duration(5 * time.Second),
		Status:  [2]int{200, 399},
	}
}

func noEnv(string) (string, bool) { return "", false }

func envWith(name, value string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		if k == name {
			return value, true
		}

		return "", false
	}
}

func mustNew(t *testing.T, e config.HTTP, opts httpcheck.Options) *httpcheck.Check {
	t.Helper()

	if opts.LookupEnv == nil {
		opts.LookupEnv = noEnv
	}

	c, err := httpcheck.New(e, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

// only returns the single reading of res, failing when there is not exactly one.
func only(t *testing.T, res health.CheckResult) health.Reading {
	t.Helper()

	if len(res.Readings) != 1 {
		t.Fatalf("readings = %+v (err %v), want exactly one", res.Readings, res.Err)
	}

	r := res.Readings[0]
	if r.Key != key || r.Kind != health.State {
		t.Fatalf("reading = %+v, want state reading %q", r, key)
	}

	return r
}

func wantVerdict(t *testing.T, res health.CheckResult, want health.Verdict, wantText string) health.Reading {
	t.Helper()

	r := only(t, res)
	if r.Verdict != want {
		t.Fatalf("verdict = %v (%q), want %v", r.Verdict, r.Text, want)
	}

	if !strings.Contains(r.Text, wantText) {
		t.Fatalf("text = %q, want it to contain %q", r.Text, wantText)
	}

	return r
}

func wantUnjudged(t *testing.T, res health.CheckResult, wantErr string) {
	t.Helper()

	if len(res.Readings) != 0 {
		t.Fatalf("readings = %+v, want none (condition not judged)", res.Readings)
	}

	if res.Err == nil || !strings.Contains(res.Err.Error(), wantErr) {
		t.Fatalf("err = %v, want it to contain %q", res.Err, wantErr)
	}
}

func TestStatusRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		rng     [2]int
		verdict health.Verdict
		text    string
	}{
		{"ok", http.StatusOK, [2]int{200, 399}, health.Good, "status 200"},
		{"lower bound", http.StatusOK, [2]int{200, 200}, health.Good, "status 200"},
		{"upper bound", http.StatusNotModified, [2]int{200, 399}, health.Good, "status 304"},
		{"server error", http.StatusServiceUnavailable, [2]int{200, 399}, health.Bad, "status 503, expected 200-399"},
		{"below range", http.StatusNoContent, [2]int{205, 299}, health.Bad, "status 204, expected 205-299"},
		{"not found", http.StatusNotFound, [2]int{200, 399}, health.Bad, "status 404"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			srv := newHTTPServer(t, statusHandler(rec, tt.status))
			e := entry(srv.URL + "/health")
			e.Status = tt.rng

			res := mustNew(t, e, httpcheck.Options{}).Run(t.Context())
			if res.Err != nil {
				t.Fatalf("err = %v", res.Err)
			}

			wantVerdict(t, res, tt.verdict, tt.text)
		})
	}
}

func TestPlainHTTPConnectToKeepsHost(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := newHTTPServer(t, statusHandler(rec, http.StatusOK))
	d := &fakeDialer{target: srv.Listener.Addr().String()}
	e := entry("http://" + appHost + "/health")
	e.ConnectTo = "192.0.2.5"

	wantVerdict(t, mustNew(t, e, httpcheck.Options{Dialer: d}).Run(t.Context()), health.Good, "status 200")

	if got := d.addresses(); len(got) != 1 || got[0] != "192.0.2.5:80" {
		t.Fatalf("dialed %v, want [192.0.2.5:80]", got)
	}

	if hosts, _, _ := rec.seen(); hosts[0] != appHost {
		t.Fatalf("Host = %q, want %q", hosts[0], appHost)
	}
}

func TestTLSConnectToKeepsHostForSNIAndVerification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		url       string
		connectTo string
		dialed    string
	}{
		{"default port", "https://" + appHost + "/health", "192.0.2.5", "192.0.2.5:443"},
		{"url port", "https://" + appHost + ":8443/health", "192.0.2.5", "192.0.2.5:8443"},
		{"explicit port", "https://" + appHost + "/health", "192.0.2.5:9443", "192.0.2.5:9443"},
		{"ipv6", "https://" + appHost + "/health", "2001:db8::5", "[2001:db8::5]:443"},
		{"bracketed ipv6", "https://" + appHost + "/health", "[2001:db8::5]", "[2001:db8::5]:443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			srv, tlsCfg := newTLSServer(t, appHost, rec, statusHandler(rec, http.StatusOK))
			d := &fakeDialer{target: srv.Listener.Addr().String()}
			e := entry(tt.url)
			e.ConnectTo = tt.connectTo

			res := mustNew(t, e, httpcheck.Options{Dialer: d, TLSConfig: tlsCfg}).Run(t.Context())
			wantVerdict(t, res, health.Good, "status 200")

			if got := d.addresses(); len(got) != 1 || got[0] != tt.dialed {
				t.Fatalf("dialed %v, want [%s]", got, tt.dialed)
			}

			hosts, _, sni := rec.seen()
			if len(sni) == 0 || sni[0] != appHost {
				t.Fatalf("SNI = %v, want %q", sni, appHost)
			}

			if !strings.HasPrefix(hosts[0], appHost) {
				t.Fatalf("Host = %q, want %q", hosts[0], appHost)
			}
		})
	}
}

func TestTLSVerificationFailureIsBad(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	// The server presents a certificate for another name.
	srv, tlsCfg := newTLSServer(t, "other.example.org", rec, statusHandler(rec, http.StatusOK))
	d := &fakeDialer{target: srv.Listener.Addr().String()}
	e := entry("https://" + appHost + "/health")
	e.ConnectTo = "192.0.2.5"

	res := mustNew(t, e, httpcheck.Options{Dialer: d, TLSConfig: tlsCfg}).Run(t.Context())
	if res.Err != nil {
		t.Fatalf("err = %v, want none", res.Err)
	}

	wantVerdict(t, res, health.Bad, "TLS")

	if rec.calls() != 0 {
		t.Fatal("request reached the handler despite a TLS failure")
	}
}

func TestDoHResolvesHostAndDialsFirstAddress(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv, tlsCfg := newTLSServer(t, appHost, rec, statusHandler(rec, http.StatusOK))
	d := &fakeDialer{target: srv.Listener.Addr().String()}
	r := &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")}}
	e := entry("https://" + appHost + "/health")
	e.DoH = "https://doh.example.org/dns-query"

	res := mustNew(t, e, httpcheck.Options{Dialer: d, Resolver: r, TLSConfig: tlsCfg}).Run(t.Context())
	wantVerdict(t, res, health.Good, "status 200")

	if len(r.hosts) != 1 || r.hosts[0] != appHost {
		t.Fatalf("resolved %v, want [%s]", r.hosts, appHost)
	}

	if got := d.addresses(); len(got) != 1 || got[0] != "203.0.113.7:443" {
		t.Fatalf("dialed %v, want [203.0.113.7:443]", got)
	}

	if hosts, _, sni := rec.seen(); sni[0] != appHost || hosts[0] != appHost {
		t.Fatalf("SNI %v Host %v, want %q", sni, hosts, appHost)
	}
}

func TestDoHFailureIsCheckerError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		addrs []netip.Addr
		err   error
	}{
		{"resolver error", nil, errors.New("doh endpoint unreachable")},
		{"no addresses", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := &fakeDialer{err: errors.New("must not dial")}
			e := entry("https://" + appHost + "/health")
			e.DoH = "https://doh.example.org/dns-query"

			res := mustNew(t, e, httpcheck.Options{Dialer: d, Resolver: &fakeResolver{addrs: tt.addrs, err: tt.err}}).
				Run(t.Context())
			wantUnjudged(t, res, "resolve "+appHost)

			if tt.err != nil && !errors.Is(res.Err, tt.err) {
				t.Fatalf("err = %v, want it to wrap the resolver error", res.Err)
			}

			if len(d.addresses()) != 0 {
				t.Fatalf("dialed %v after a failed resolution", d.addresses())
			}
		})
	}
}

func TestTransportFailuresAreBad(t *testing.T) {
	t.Parallel()

	// A listener that is closed at once gives a port that refuses connections.
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	refused := ln.Addr().String()
	_ = ln.Close()

	dnsErr := &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
		Err: "no such host", Name: appHost, IsNotFound: true,
	}}

	tests := []struct {
		name string
		d    *fakeDialer
		text string
	}{
		{"refused", &fakeDialer{target: refused}, "connection refused"},
		{"dns", &fakeDialer{err: dnsErr}, "DNS lookup of " + appHost + " failed: no such host"},
		{"other", &fakeDialer{err: errors.New("network is unreachable")}, "network is unreachable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := mustNew(t, entry("http://"+appHost+"/health"), httpcheck.Options{Dialer: tt.d}).Run(t.Context())
			if res.Err != nil {
				t.Fatalf("err = %v, want none", res.Err)
			}

			wantVerdict(t, res, health.Bad, tt.text)
		})
	}
}

func TestTimeoutIsBad(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := newHTTPServer(t, http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })

	e := entry(srv.URL)
	e.Timeout = config.Duration(50 * time.Millisecond)

	res := mustNew(t, e, httpcheck.Options{}).Run(t.Context())
	if res.Err != nil {
		t.Fatalf("err = %v, want none", res.Err)
	}

	wantVerdict(t, res, health.Bad, "no answer within 50ms")
}

func TestCancelledRunIsNotJudged(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := newHTTPServer(t, statusHandler(rec, http.StatusOK))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	wantUnjudged(t, mustNew(t, entry(srv.URL), httpcheck.Options{}).Run(ctx), "canceled")
}

func TestRedirects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		hops    int
		verdict health.Verdict
		text    string
	}{
		{"none", 0, health.Good, "status 200"},
		{"five", 5, health.Good, "status 200"},
		{"six", 6, health.Bad, "more than 5 redirects"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			srv := newHTTPServer(t, redirectHandler(rec))

			e := entry(srv.URL + "/hop/" + strconv.Itoa(tt.hops))
			e.FollowRedirects = true

			res := mustNew(t, e, httpcheck.Options{}).Run(t.Context())
			wantVerdict(t, res, tt.verdict, tt.text)
		})
	}
}

func TestRedirectToSameHostKeepsConnectTo(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := newHTTPServer(t, redirectHandler(rec))
	d := &fakeDialer{target: srv.Listener.Addr().String()}
	e := entry("http://" + appHost + "/hop/2")
	e.ConnectTo = "192.0.2.5"
	e.FollowRedirects = true

	wantVerdict(t, mustNew(t, e, httpcheck.Options{Dialer: d}).Run(t.Context()), health.Good, "status 200")

	for _, a := range d.addresses() {
		if a != "192.0.2.5:80" {
			t.Fatalf("dialed %v, want only 192.0.2.5:80", d.addresses())
		}
	}

	hosts, _, _ := rec.seen()
	for _, h := range hosts {
		if h != appHost {
			t.Fatalf("Host headers %v, want all %q", hosts, appHost)
		}
	}
}

func TestConnectToAppliesOnlyToURLHost(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := newHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)

		if req.Host == appHost {
			http.Redirect(w, req, "http://other.example.org/landing", http.StatusFound)

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	d := &fakeDialer{target: srv.Listener.Addr().String()}
	e := entry("http://" + appHost + "/")
	e.ConnectTo = "192.0.2.5"
	e.FollowRedirects = true

	wantVerdict(t, mustNew(t, e, httpcheck.Options{Dialer: d}).Run(t.Context()), health.Good, "status 200")

	want := []string{"192.0.2.5:80", "other.example.org:80"}
	if got := d.addresses(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dialed %v, want %v", got, want)
	}
}

func TestHeaderFromEnv(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := newHTTPServer(t, statusHandler(rec, http.StatusOK))
	e := entry(srv.URL)
	e.Header = &config.Header{Name: "Authorization", ValueEnv: tokenEnv, Prefix: "Bearer "}

	res := mustNew(t, e, httpcheck.Options{LookupEnv: envWith(tokenEnv, secret)}).Run(t.Context())
	r := wantVerdict(t, res, health.Good, "status 200")

	if _, auth, _ := rec.seen(); auth[0] != "Bearer "+secret {
		t.Fatalf("Authorization = %q, want the prefixed env value", auth[0])
	}

	if strings.Contains(r.Text, secret) {
		t.Fatalf("reading text %q leaks the secret", r.Text)
	}
}

func TestHeaderSecretNeverInBadText(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := newHTTPServer(t, statusHandler(rec, http.StatusUnauthorized))
	e := entry(srv.URL)
	e.Header = &config.Header{Name: "Authorization", ValueEnv: tokenEnv, Prefix: "Bearer "}

	r := wantVerdict(t, mustNew(t, e, httpcheck.Options{LookupEnv: envWith(tokenEnv, secret)}).Run(t.Context()),
		health.Bad, "status 401")
	if strings.Contains(r.Text, secret) {
		t.Fatalf("reading text %q leaks the secret", r.Text)
	}
}

func TestBadSecretIsCheckerError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		env   func(string) (string, bool)
		value string
		want  string
	}{
		{"missing", noEnv, "", "APP_TOKEN is not set"},
		{"empty", envWith(tokenEnv, ""), "", "APP_TOKEN is empty"},
		{"blank", envWith(tokenEnv, "   "), "", "APP_TOKEN is empty"},
		{"changeme", envWith(tokenEnv, "changeme"), "changeme", "placeholder"},
		{"CHANGE_ME", envWith(tokenEnv, "CHANGE_ME"), "CHANGE_ME", "placeholder"},
		{"angle brackets", envWith(tokenEnv, "<api-token>"), "<api-token>", "placeholder"},
		{"xxx", envWith(tokenEnv, "xxx"), "xxx", "placeholder"},
		{"many x", envWith(tokenEnv, "XXXXXXXXXX"), "XXXXXXXXXX", "placeholder"},
		{"todo", envWith(tokenEnv, "TODO"), "TODO", "placeholder"},
		{"template", envWith(tokenEnv, "${APP_TOKEN}"), "${APP_TOKEN}", "placeholder"},
		{"your token here", envWith(tokenEnv, "your-token-here"), "your-token-here", "placeholder"},
		{"stars", envWith(tokenEnv, "********"), "********", "placeholder"},
		{"newline", envWith(tokenEnv, "abc\r\nX-Evil: 1"), "abc", "control character"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			srv := newHTTPServer(t, statusHandler(rec, http.StatusOK))
			e := entry(srv.URL)
			e.Header = &config.Header{Name: "Authorization", ValueEnv: tokenEnv, Prefix: "Bearer "}

			res := mustNew(t, e, httpcheck.Options{LookupEnv: tt.env}).Run(t.Context())
			wantUnjudged(t, res, tt.want)

			if !errors.Is(res.Err, httpcheck.ErrSecret) {
				t.Fatalf("err = %v, want ErrSecret", res.Err)
			}

			if tt.value != "" && strings.Contains(res.Err.Error(), tt.value) {
				t.Fatalf("err %q leaks the value", res.Err)
			}

			if rec.calls() != 0 {
				t.Fatal("request sent without a usable secret")
			}
		})
	}
}

func TestRealLookingSecretsAreAccepted(t *testing.T) {
	t.Parallel()

	for _, v := range []string{
		"xoxb-1234-abcd", "ghp_0123456789abcdef", "Xx9-token-like", "k9-here-Z7q", "aZ_here_q1", "q8your-token",
	} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			srv := newHTTPServer(t, statusHandler(rec, http.StatusOK))
			e := entry(srv.URL)
			e.Header = &config.Header{Name: "X-Api-Key", ValueEnv: tokenEnv}

			wantVerdict(t, mustNew(t, e, httpcheck.Options{LookupEnv: envWith(tokenEnv, v)}).Run(t.Context()),
				health.Good, "status 200")
		})
	}
}

func TestContainerRequirement(t *testing.T) {
	t.Parallel()

	lookupErr := errors.New("docker socket unavailable")

	tests := []struct {
		name       string
		status     int
		containers *fakeContainers
		verdict    health.Verdict // Unknown means "no reading"
		texts      []string
		err        string
	}{
		{"healthy and up", http.StatusOK, &fakeContainers{healthy: true}, health.Good, []string{"status 200"}, ""},
		{
			"unhealthy, endpoint up", http.StatusOK, &fakeContainers{}, health.Bad,
			[]string{"container app is not healthy"},
			"",
		},
		{
			"unhealthy, endpoint down", http.StatusBadGateway, &fakeContainers{}, health.Bad,
			[]string{"status 502", "container app is not healthy"},
			"",
		},
		{
			"lookup error, endpoint up", http.StatusOK, &fakeContainers{err: lookupErr}, health.Unknown, nil,
			"container app: docker socket unavailable",
		},
		{
			"lookup error, endpoint down", http.StatusBadGateway, &fakeContainers{err: lookupErr}, health.Bad,
			[]string{"status 502"},
			"container app: docker socket unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			srv := newHTTPServer(t, statusHandler(rec, tt.status))
			e := entry(srv.URL)
			e.Container = "app"

			res := mustNew(t, e, httpcheck.Options{Containers: tt.containers}).Run(t.Context())

			if len(tt.containers.names) != 1 || tt.containers.names[0] != "app" {
				t.Fatalf("container lookups = %v, want [app]", tt.containers.names)
			}

			if !tt.containers.deadline {
				t.Fatal("container lookup ran without the entry timeout")
			}

			checkCombined(t, res, tt.verdict, tt.texts, tt.err)
		})
	}
}

// checkCombined verifies a result: verdict Unknown means no reading, and an
// empty wantErr means no error.
func checkCombined(t *testing.T, res health.CheckResult, verdict health.Verdict, texts []string, wantErr string) {
	t.Helper()

	if wantErr == "" && res.Err != nil {
		t.Fatalf("err = %v, want none", res.Err)
	}

	if wantErr != "" && (res.Err == nil || !strings.Contains(res.Err.Error(), wantErr)) {
		t.Fatalf("err = %v, want it to contain %q", res.Err, wantErr)
	}

	if verdict == health.Unknown {
		if len(res.Readings) != 0 {
			t.Fatalf("readings = %+v, want none", res.Readings)
		}

		return
	}

	r := only(t, res)
	if r.Verdict != verdict {
		t.Fatalf("verdict = %v (%q), want %v", r.Verdict, r.Text, verdict)
	}

	for _, want := range texts {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("text = %q, want it to contain %q", r.Text, want)
		}
	}
}

func TestNewRejectsIncompleteSetup(t *testing.T) {
	t.Parallel()

	withDoH := entry("https://" + appHost + "/")
	withDoH.DoH = "https://doh.example.org/dns-query"

	withContainer := entry("https://" + appHost + "/")
	withContainer.Container = "app"

	badPrefix := entry("https://" + appHost + "/")
	badPrefix.Header = &config.Header{Name: "Authorization", ValueEnv: tokenEnv, Prefix: "Bearer\n"}

	tests := []struct {
		name string
		e    config.HTTP
		want string
	}{
		{"control character in prefix", badPrefix, "prefix contains a control character"},
		{"bad url", entry("://nope"), "url"},
		{"relative url", entry("/health"), "url"},
		{"doh without resolver", withDoH, "resolver"},
		{"container without source", withContainer, "container"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := httpcheck.New(tt.e, httpcheck.Options{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestKey(t *testing.T) {
	t.Parallel()

	if got := mustNew(t, entry("https://"+appHost+"/"), httpcheck.Options{}).Key(); got != key {
		t.Fatalf("Key = %q, want %q", got, key)
	}
}

func TestSecretHeaderNotForwardedToOtherHost(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := newHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)

		switch {
		case req.Host == appHost && req.URL.Path == "/":
			http.Redirect(w, req, "/same", http.StatusFound)
		case req.Host == appHost:
			http.Redirect(w, req, "http://other.example.org/landing", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	e := entry("http://" + appHost + "/")
	e.Header = &config.Header{Name: "X-Api-Key", ValueEnv: tokenEnv}
	e.FollowRedirects = true

	res := mustNew(t, e, httpcheck.Options{
		Dialer:    &fakeDialer{target: srv.Listener.Addr().String()},
		LookupEnv: envWith(tokenEnv, secret),
	}).Run(t.Context())
	wantVerdict(t, res, health.Good, "status 200")

	if got := rec.apiKeys(appHost); !slices.Equal(got, []string{secret, secret}) {
		t.Fatalf("URL host saw X-Api-Key %d times correctly, want 2 (got %d values)", countEqual(got, secret), len(got))
	}

	if got := rec.apiKeys("other.example.org"); len(got) != 1 || got[0] != "" {
		t.Fatalf("other host saw X-Api-Key in %d of %d requests, want none", countEqual(got, secret), len(got))
	}
}

func TestSecretHeaderNotForwardedOnDowngrade(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	tlsSrv, tlsCfg := newTLSServer(t, appHost, rec, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)
		http.Redirect(w, req, "http://"+appHost+"/plain", http.StatusFound)
	}))

	plainKeys := &recorder{}
	plain := newHTTPServer(t, statusHandler(plainKeys, http.StatusOK))

	d := &fakeDialer{byPort: map[string]string{
		"443": tlsSrv.Listener.Addr().String(),
		"80":  plain.Listener.Addr().String(),
	}}
	e := entry("https://" + appHost + "/")
	e.Header = &config.Header{Name: "X-Api-Key", ValueEnv: tokenEnv}
	e.FollowRedirects = true

	res := mustNew(t, e, httpcheck.Options{Dialer: d, TLSConfig: tlsCfg, LookupEnv: envWith(tokenEnv, secret)}).
		Run(t.Context())
	wantVerdict(t, res, health.Good, "status 200")

	if got := rec.apiKeys(appHost); len(got) != 1 || got[0] != secret {
		t.Fatalf("https hop saw the header %d of %d times, want 1 of 1", countEqual(got, secret), len(got))
	}

	if got := plainKeys.apiKeys(appHost); len(got) != 1 || got[0] != "" {
		t.Fatalf("plain http hop saw the header in %d of %d requests, want none", countEqual(got, secret), len(got))
	}
}

func TestSlowDoHIsCheckerError(t *testing.T) {
	t.Parallel()

	r := &blockingResolver{done: make(chan struct{})}
	d := &fakeDialer{err: errors.New("must not dial")}
	e := entry("https://" + appHost + "/health")
	e.DoH = "https://doh.example.org/dns-query"
	e.Timeout = config.Duration(50 * time.Millisecond)

	res := mustNew(t, e, httpcheck.Options{Dialer: d, Resolver: r}).Run(t.Context())
	wantUnjudged(t, res, "resolve "+appHost)

	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the DoH lookup was not cancelled at the entry timeout")
	}
}

func TestDialIsBoundedByTimeout(t *testing.T) {
	t.Parallel()

	d := &fakeDialer{block: true, done: make(chan struct{})}
	e := entry("http://" + appHost + "/health")
	e.Timeout = config.Duration(50 * time.Millisecond)

	res := mustNew(t, e, httpcheck.Options{Dialer: d}).Run(t.Context())
	wantVerdict(t, res, health.Bad, "no answer within 50ms")

	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the dial was not cancelled at the entry timeout")
	}
}

// TestHangPastTheRunDeadlineIsBad: an endpoint that does not answer before
// the run's deadline is judged Bad before that deadline, even when the entry's
// own timeout is longer, so the runner never abandons the run.
func TestHangPastTheRunDeadlineIsBad(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := newHTTPServer(t, http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	res := mustNew(t, entry(srv.URL), httpcheck.Options{}).Run(ctx)
	if ctx.Err() != nil {
		t.Fatal("the run returned only after its deadline")
	}
	if res.Err != nil {
		t.Fatalf("err = %v, want none", res.Err)
	}

	wantVerdict(t, res, health.Bad, "no answer before the run deadline")
}

// TestInvalidURLErrorDoesNotEchoTheURL: the start error is pushed to Telegram
// and email, so it never repeats the url (its userinfo or query may be secret).
func TestInvalidURLErrorDoesNotEchoTheURL(t *testing.T) {
	t.Parallel()

	const secret = "s3cret-token"
	for _, raw := range []string{
		"https://user:" + secret + "@" + appHost + ":bad/",
		"https://" + appHost + "/%zz?key=" + secret,
		"ftp://user:" + secret + "@" + appHost + "/",
	} {
		_, err := httpcheck.New(entry(raw), httpcheck.Options{})
		if err == nil || !strings.Contains(err.Error(), "url") {
			t.Fatalf("err = %v, want an invalid url", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("the error echoes the url")
		}
	}
}

// countEqual counts the values equal to want, so failures never print a secret.
func countEqual(values []string, want string) int {
	n := 0

	for _, v := range values {
		if v == want {
			n++
		}
	}

	return n
}

// TestRedirectNotFollowed proves US5 acceptance scenario 3: by default a
// redirect is not followed and the first response is judged against status.
func TestRedirectNotFollowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  [2]int
		verdict health.Verdict
		text    string
	}{
		{"redirect inside the default range is up", [2]int{200, 399}, health.Good, "status 302"},
		{"redirect outside the range is down", [2]int{200, 299}, health.Bad, "status 302, expected 200-299"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			srv := newHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				rec.record(req)

				if req.URL.Path == "/" {
					http.Redirect(w, req, "http://example.org/login", http.StatusFound)

					return
				}

				w.WriteHeader(http.StatusOK)
			}))
			d := &fakeDialer{target: srv.Listener.Addr().String()}
			e := entry("http://" + appHost + "/")
			e.Status = tt.status

			wantVerdict(t, mustNew(t, e, httpcheck.Options{Dialer: d}).Run(t.Context()), tt.verdict, tt.text)

			if got := d.addresses(); len(got) != 1 || got[0] != appHost+":80" {
				t.Fatalf("dialed %v, want only the URL host (redirect not followed)", got)
			}

			if rec.calls() != 1 {
				t.Fatalf("server saw %d requests, want 1", rec.calls())
			}
		})
	}
}

func TestMissingContainerDown(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := newHTTPServer(t, statusHandler(rec, http.StatusOK))
	e := entry(srv.URL)
	e.Container = "web"
	ctrs := &fakeContainers{err: fmt.Errorf("docker: %w", httpcheck.ErrContainerNotFound)}

	res := mustNew(t, e, httpcheck.Options{Containers: ctrs}).Run(t.Context())
	wantVerdict(t, res, health.Bad, "container web does not exist")

	if res.Err != nil {
		t.Fatalf("err = %v, want none: a missing container is judged, not unreadable", res.Err)
	}
}

func TestAuthRefusalCheckerError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mode    config.AuthRefusal
		status  int
		wantErr string // empty: a Bad reading is expected instead
		badText string
	}{
		{
			"401 checker error",
			config.AuthRefusalCheckerError,
			http.StatusUnauthorized,
			"endpoint refused the credentials (401)",
			"",
		},
		{
			"403 checker error",
			config.AuthRefusalCheckerError,
			http.StatusForbidden,
			"endpoint refused the credentials (403)",
			"",
		},
		{
			"500 stays down",
			config.AuthRefusalCheckerError,
			http.StatusInternalServerError,
			"",
			"status 500, expected 200-399",
		},
		{"401 down by default", config.AuthRefusalDown, http.StatusUnauthorized, "", "status 401, expected 200-399"},
		{"403 down when unset", "", http.StatusForbidden, "", "status 403, expected 200-399"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			srv := newHTTPServer(t, statusHandler(rec, tt.status))
			e := entry(srv.URL)
			e.AuthRefusal = tt.mode
			e.Header = &config.Header{Name: "Authorization", ValueEnv: tokenEnv, Prefix: "Bearer "}

			res := mustNew(t, e, httpcheck.Options{LookupEnv: envWith(tokenEnv, secret)}).Run(t.Context())

			if tt.wantErr == "" {
				wantVerdict(t, res, health.Bad, tt.badText)

				return
			}

			wantUnjudged(t, res, tt.wantErr)

			ces := health.CheckErrors(res.Err)
			if len(ces) != 1 || ces[0].Key(key) != key+"/auth" {
				t.Fatalf("check errors = %+v, want one keyed %s/auth", ces, key)
			}

			if strings.Contains(res.Err.Error(), secret) {
				t.Fatalf("err %q leaks the secret", res.Err)
			}
		})
	}
}

func TestExtraPlaceholder(t *testing.T) {
	t.Parallel()

	markers := []string{"sample-key", "Not-Deployed"}

	tests := []struct {
		name  string
		value string
		ok    bool
	}{
		{"exact marker", "sample-key", false},
		{"marker in another case", "SAMPLE-KEY", false},
		{"marker inside the value", "abc-Sample-Key-123", false},
		{"marker given in mixed case", "not-deployed", false},
		{"built-in word still rejected", "changeme", false},
		{"real-looking secret", secret, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := config.Header{Name: "Authorization", ValueEnv: tokenEnv, Prefix: "Bearer ", Placeholders: markers}

			got, err := httpcheck.HeaderSecret(&h, envWith(tokenEnv, tt.value))
			if tt.ok {
				if err != nil || got != "Bearer "+tt.value {
					t.Fatalf("HeaderSecret = %q, %v, want the prefixed value", got, err)
				}

				return
			}

			if !errors.Is(err, httpcheck.ErrSecret) || !strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("HeaderSecret err = %v, want ErrSecret naming a placeholder", err)
			}

			if strings.Contains(err.Error(), tt.value) {
				t.Fatalf("err %q leaks the value", err)
			}

			// The check itself uses the same rule: no request, no reading.
			rec := &recorder{}
			srv := newHTTPServer(t, statusHandler(rec, http.StatusOK))
			e := entry(srv.URL)
			e.Header = &h

			wantUnjudged(t, mustNew(t, e, httpcheck.Options{LookupEnv: envWith(tokenEnv, tt.value)}).Run(t.Context()),
				"placeholder")

			if rec.calls() != 0 {
				t.Fatalf("server saw %d requests with a placeholder secret, want 0", rec.calls())
			}
		})
	}
}

func TestHeaderSecretWithoutHeader(t *testing.T) {
	t.Parallel()

	if got, err := httpcheck.HeaderSecret(nil, noEnv); got != "" || err != nil {
		t.Fatalf("HeaderSecret(nil) = %q, %v, want empty and no error", got, err)
	}
}

// dohEntry is an entry for appHost that resolves through DoH.
func dohEntry() config.HTTP {
	e := entry("https://" + appHost + "/health")
	e.DoH = "https://doh.example.org/dns-query"

	return e
}

// TestDoHNXDomainDown proves US5 acceptance scenario 6 (first half): a
// resolver that answers that the name does not exist makes the endpoint down.
func TestDoHNXDomainDown(t *testing.T) {
	t.Parallel()

	d := &fakeDialer{err: errors.New("must not dial")}
	r := &fakeResolver{err: fmt.Errorf("lookup: %w", httpcheck.ErrNameNotFound)}

	res := mustNew(t, dohEntry(), httpcheck.Options{Dialer: d, Resolver: r}).Run(t.Context())
	wantVerdict(t, res, health.Bad, "name "+appHost+" does not exist")

	if res.Err != nil {
		t.Fatalf("err = %v, want none: NXDOMAIN is a judgement", res.Err)
	}

	if len(d.addresses()) != 0 {
		t.Fatalf("dialed %v after NXDOMAIN", d.addresses())
	}
}

func TestDoHNoDataDown(t *testing.T) {
	t.Parallel()

	d := &fakeDialer{err: errors.New("must not dial")}
	r := &fakeResolver{err: fmt.Errorf("lookup: %w", httpcheck.ErrNoAddress)}

	res := mustNew(t, dohEntry(), httpcheck.Options{Dialer: d, Resolver: r}).Run(t.Context())
	wantVerdict(t, res, health.Bad, "name "+appHost+" has no address")

	if res.Err != nil {
		t.Fatalf("err = %v, want none: NODATA is a judgement", res.Err)
	}
}

// TestDoHUnreachableUnknown proves US5 acceptance scenario 6 (second half):
// any resolver failure other than a definitive negative answer is unreadable.
func TestDoHUnreachableUnknown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"resolver unreachable", errors.New("send request: connection refused")},
		{"SERVFAIL", errors.New("doh: A query: DNS error RCodeServerFailure")},
		{"non-200 status", errors.New("unexpected HTTP status 502 Bad Gateway")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := &fakeDialer{err: errors.New("must not dial")}
			r := &fakeResolver{err: tt.err}

			res := mustNew(t, dohEntry(), httpcheck.Options{Dialer: d, Resolver: r}).Run(t.Context())
			wantUnjudged(t, res, "resolve "+appHost)

			if !errors.Is(res.Err, tt.err) {
				t.Fatalf("err = %v, want it to wrap the resolver error", res.Err)
			}
		})
	}
}
