// Package doh is a minimal RFC 8484 DNS-over-HTTPS resolver. It sends
// wire-format queries with POST and returns the A and AAAA addresses of a
// host. Answers are used once; TTLs are ignored.
package doh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	contentType = "application/dns-message"
	// maxResponse bounds how much of a response body is read.
	maxResponse = 64 * 1024
)

// ErrNoAddresses is returned when a name resolves without any A or AAAA record.
var ErrNoAddresses = errors.New("doh: no addresses found")

// ErrNXDomain is returned when the resolver answered that the name does not
// exist (RCODE NXDOMAIN). Like ErrNoAddresses it is a definitive answer: it is
// only reported when both the A and the AAAA query were answered.
var ErrNXDomain = errors.New("doh: name does not exist")

// Resolver resolves host names through a DoH endpoint.
type Resolver struct {
	endpoint string
	client   *http.Client
}

// New returns a Resolver for the DoH endpoint URL. A nil client selects
// http.DefaultClient. The client is copied and the copy never follows
// redirects, so a redirecting endpoint yields a clean non-200 failure.
func New(endpoint string, client *http.Client) *Resolver {
	if client == nil {
		client = http.DefaultClient
	}

	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &Resolver{endpoint: endpoint, client: &c}
}

// Lookup returns the A addresses of host followed by its AAAA addresses.
// An IP literal is returned as is. The lookup honours ctx cancellation and
// deadline. It fails only when no address could be obtained. The failure is
// ErrNXDomain or ErrNoAddresses only when both queries were answered (NXDOMAIN
// from either one, or both empty); a transport error, a non-200 status,
// SERVFAIL or REFUSED yields an ordinary error.
func (r *Resolver) Lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}

	if host == "" {
		return nil, errors.New("doh: empty host name")
	}

	name, err := dnsmessage.NewName(fqdn(host))
	if err != nil {
		return nil, fmt.Errorf("doh: invalid name %q: %w", host, err)
	}

	var (
		addrs    []netip.Addr
		errs     []error
		nxDomain bool
	)

	for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		found, err := r.query(ctx, name, typ)
		if errors.Is(err, ErrNXDomain) {
			nxDomain = true

			continue
		}

		if err != nil {
			errs = append(errs, fmt.Errorf("%s query for %q: %w", typ, host, err))

			continue
		}

		addrs = append(addrs, found...)
	}

	if len(addrs) > 0 {
		return addrs, nil
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("doh: %w", errors.Join(errs...))
	}

	if nxDomain {
		return nil, fmt.Errorf("%w: %q", ErrNXDomain, host)
	}

	return nil, fmt.Errorf("%w for %q", ErrNoAddresses, host)
}

func (r *Resolver) query(ctx context.Context, name dnsmessage.Name, typ dnsmessage.Type) ([]netip.Addr, error) {
	// RFC 8484 section 4.1: the ID SHOULD be 0 to be cache friendly.
	msg := dnsmessage.Message{
		Questions: []dnsmessage.Question{
			{Name: name, Type: typ, Class: dnsmessage.ClassINET},
		},

		RecursionDesired: true,
	}

	packed, err := msg.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(packed))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	body, err := r.post(req)
	if err != nil {
		return nil, err
	}

	return parseAnswer(body, msg.Questions[0])
}

// post sends the request and returns the validated DoH response body.
func (r *Resolver) post(req *http.Request) ([]byte, error) {
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", contentType)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}

	if mt, _, perr := mime.ParseMediaType(resp.Header.Get("Content-Type")); perr != nil || mt != contentType {
		return nil, fmt.Errorf("unexpected content type %q", resp.Header.Get("Content-Type"))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if len(body) > maxResponse {
		return nil, fmt.Errorf("response too large (over %d bytes)", maxResponse)
	}

	return body, nil
}

// parseAnswer decodes and validates a response against the question sent.
func parseAnswer(body []byte, question dnsmessage.Question) ([]netip.Addr, error) {
	var answer dnsmessage.Message
	if err := answer.Unpack(body); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if !answer.Response {
		return nil, errors.New("reply is not a response")
	}

	if answer.Truncated {
		return nil, errors.New("truncated response")
	}

	if len(answer.Questions) != 1 || !sameQuestion(answer.Questions[0], question) {
		return nil, errors.New("response question does not match the query")
	}

	if answer.RCode == dnsmessage.RCodeNameError {
		return nil, ErrNXDomain
	}

	if answer.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf("DNS error %s", answer.RCode)
	}

	return addresses(answer.Answers, question.Name, question.Type), nil
}

func sameQuestion(a, b dnsmessage.Question) bool {
	return a.Type == b.Type && a.Class == b.Class && sameName(a.Name, b.Name)
}

func sameName(a, b dnsmessage.Name) bool {
	return strings.EqualFold(a.String(), b.String())
}

// addresses extracts the records of the wanted type whose owner is the
// queried name or a target of a CNAME chain starting at it.
func addresses(records []dnsmessage.Resource, name dnsmessage.Name, typ dnsmessage.Type) []netip.Addr {
	owners := chainOwners(records, name)

	var out []netip.Addr

	for i := range records {
		if !owners[key(records[i].Header.Name)] {
			continue
		}

		switch body := records[i].Body.(type) {
		case *dnsmessage.AResource:
			if typ == dnsmessage.TypeA {
				out = append(out, netip.AddrFrom4(body.A))
			}
		case *dnsmessage.AAAAResource:
			if typ == dnsmessage.TypeAAAA {
				out = append(out, netip.AddrFrom16(body.AAAA))
			}
		}
	}

	return out
}

// chainOwners returns the lower-cased names reachable from name by CNAMEs.
func chainOwners(records []dnsmessage.Resource, name dnsmessage.Name) map[string]bool {
	owners := map[string]bool{key(name): true}

	for grew := true; grew; {
		grew = false

		for i := range records {
			cname, ok := records[i].Body.(*dnsmessage.CNAMEResource)
			if !ok || !owners[key(records[i].Header.Name)] || owners[key(cname.CNAME)] {
				continue
			}

			owners[key(cname.CNAME)] = true
			grew = true
		}
	}

	return owners
}

func key(n dnsmessage.Name) string { return strings.ToLower(n.String()) }

func fqdn(host string) string {
	if host != "" && host[len(host)-1] == '.' {
		return host
	}

	return host + "."
}
