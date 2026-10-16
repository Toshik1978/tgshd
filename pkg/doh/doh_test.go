package doh_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/Toshik1978/tgshd/pkg/doh"
)

const contentType = "application/dns-message"

// reply is what the fake server answers for one query type.
type reply struct {
	rcode   dnsmessage.RCode
	records []dnsmessage.Resource
}

func aRecord(name string, ip [4]byte) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60,
		},
		Body: &dnsmessage.AResource{A: ip},
	}
}

func aaaaRecord(name string, ip [16]byte) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 60,
		},
		Body: &dnsmessage.AAAAResource{AAAA: ip},
	}
}

func cnameRecord(name, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 60,
		},
		Body: &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)},
	}
}

func v6Addr() [16]byte {
	return [16]byte{0x20, 0x01, 0x0d, 0xb8, 15: 1}
}

func dnsServer(t *testing.T, replies map[dnsmessage.Type]reply) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != contentType ||
			r.Header.Get("Accept") != contentType {
			http.Error(w, "bad request", http.StatusBadRequest)

			return
		}

		body, _ := io.ReadAll(r.Body)

		var q dnsmessage.Message
		if err := q.Unpack(body); err != nil || len(q.Questions) != 1 || q.ID != 0 {
			http.Error(w, "bad query", http.StatusBadRequest)

			return
		}

		rep := replies[q.Questions[0].Type]
		resp := dnsmessage.Message{
			Questions: q.Questions, Answers: rep.records,
			Response: true,
			RCode:    rep.rcode,
		}

		out, err := resp.Pack()
		if err != nil {
			t.Errorf("pack: %v", err)
		}

		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestLookup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		replies map[dnsmessage.Type]reply
		want    []string
		wantErr bool
	}{
		{
			name: "A and AAAA",
			replies: map[dnsmessage.Type]reply{
				dnsmessage.TypeA: {
					records: []dnsmessage.Resource{aRecord("www.example.org.", [4]byte{192, 0, 2, 1})},
				},
				dnsmessage.TypeAAAA: {records: []dnsmessage.Resource{aaaaRecord("www.example.org.", v6Addr())}},
			},
			want: []string{"192.0.2.1", "2001:db8::1"},
		},
		{
			name: "A only",
			replies: map[dnsmessage.Type]reply{
				dnsmessage.TypeA: {
					records: []dnsmessage.Resource{aRecord("www.example.org.", [4]byte{198, 51, 100, 7})},
				},
			},
			want: []string{"198.51.100.7"},
		},
		{
			name: "AAAA only",
			replies: map[dnsmessage.Type]reply{
				dnsmessage.TypeAAAA: {records: []dnsmessage.Resource{aaaaRecord("www.example.org.", v6Addr())}},
			},
			want: []string{"2001:db8::1"},
		},
		{
			name: "CNAME chain with A",
			replies: map[dnsmessage.Type]reply{
				dnsmessage.TypeA: {records: []dnsmessage.Resource{
					cnameRecord("www.example.org.", "edge.example.org."),
					aRecord("edge.example.org.", [4]byte{203, 0, 113, 9}),
				}},
			},
			want: []string{"203.0.113.9"},
		},
		{
			name: "NXDOMAIN",
			replies: map[dnsmessage.Type]reply{
				dnsmessage.TypeA:    {rcode: dnsmessage.RCodeNameError},
				dnsmessage.TypeAAAA: {rcode: dnsmessage.RCodeNameError},
			},
			wantErr: true,
		},
		{
			name: "SERVFAIL",
			replies: map[dnsmessage.Type]reply{
				dnsmessage.TypeA:    {rcode: dnsmessage.RCodeServerFailure},
				dnsmessage.TypeAAAA: {rcode: dnsmessage.RCodeServerFailure},
			},
			wantErr: true,
		},
		{
			name:    "NOERROR without answers",
			replies: map[dnsmessage.Type]reply{},
			wantErr: true,
		},
		{
			name: "A fails but AAAA answers",
			replies: map[dnsmessage.Type]reply{
				dnsmessage.TypeA:    {rcode: dnsmessage.RCodeServerFailure},
				dnsmessage.TypeAAAA: {records: []dnsmessage.Resource{aaaaRecord("www.example.org.", v6Addr())}},
			},
			want: []string{"2001:db8::1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := dnsServer(t, tt.replies)
			got, err := doh.New(srv.URL, srv.Client()).Lookup(context.Background(), "www.example.org")

			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}

				return
			}

			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}

			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}

			for i, w := range tt.want {
				if got[i] != netip.MustParseAddr(w) {
					t.Errorf("addr %d = %v, want %v", i, got[i], w)
				}
			}
		})
	}
}

func TestLookupIPLiteral(t *testing.T) {
	t.Parallel()

	got, err := doh.New("http://127.0.0.1:1", nil).Lookup(context.Background(), "192.0.2.5")
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("192.0.2.5") {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestLookupHTTPFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"non-200", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusBadGateway) }},
		{"wrong content type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html></html>"))
		}},
		{"truncated body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write([]byte{0, 0, 0x81, 0x80, 0, 1})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			if got, err := doh.New(srv.URL, srv.Client()).Lookup(context.Background(), "www.example.org"); err == nil {
				t.Fatalf("want error, got %v", got)
			}
		})
	}
}

func TestLookupContextTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-release }))

	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := doh.New(srv.URL, srv.Client()).Lookup(ctx, "www.example.org")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestLookupInvalidName(t *testing.T) {
	t.Parallel()

	if _, err := doh.New("http://127.0.0.1:1", nil).Lookup(context.Background(), ""); err == nil {
		t.Fatal("want error for empty name")
	}
}

// rawServer serves whatever respond builds from the parsed query.
func rawServer(t *testing.T, respond func(q dnsmessage.Message) []byte) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var q dnsmessage.Message
		if err := q.Unpack(body); err != nil {
			http.Error(w, "bad query", http.StatusBadRequest)

			return
		}

		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(respond(q))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestLookupRejectsBadResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		respond func(q dnsmessage.Message) []byte
	}{
		{"answer for another name", func(q dnsmessage.Message) []byte {
			m := dnsmessage.Message{
				Questions: q.Questions,
				Answers:   []dnsmessage.Resource{aRecord("other.example.org.", [4]byte{192, 0, 2, 66})},

				Response: true,
			}
			out, _ := m.Pack()

			return out
		}},
		{"CNAME to unrelated chain", func(q dnsmessage.Message) []byte {
			m := dnsmessage.Message{
				Questions: q.Questions,
				Answers: []dnsmessage.Resource{
					cnameRecord("elsewhere.example.org.", "edge.example.org."),
					aRecord("edge.example.org.", [4]byte{192, 0, 2, 66}),
				},

				Response: true,
			}
			out, _ := m.Pack()

			return out
		}},
		{"mismatched question", func(q dnsmessage.Message) []byte {
			m := dnsmessage.Message{
				Questions: []dnsmessage.Question{
					{
						Name: dnsmessage.MustNewName(
							"other.example.org.",
						),
						Type:  q.Questions[0].Type,
						Class: dnsmessage.ClassINET,
					},
				},
				Answers: []dnsmessage.Resource{aRecord("other.example.org.", [4]byte{192, 0, 2, 66})},

				Response: true,
			}
			out, _ := m.Pack()

			return out
		}},
		{"no question", func(dnsmessage.Message) []byte {
			m := dnsmessage.Message{
				Answers:  []dnsmessage.Resource{aRecord("www.example.org.", [4]byte{192, 0, 2, 66})},
				Response: true,
			}
			out, _ := m.Pack()

			return out
		}},
		{"not a response", func(q dnsmessage.Message) []byte {
			m := dnsmessage.Message{
				Questions: q.Questions,
				Answers:   []dnsmessage.Resource{aRecord("www.example.org.", [4]byte{192, 0, 2, 66})},
			}
			out, _ := m.Pack()

			return out
		}},
		{"truncated flag", func(q dnsmessage.Message) []byte {
			m := dnsmessage.Message{
				Questions: q.Questions,
				Answers:   []dnsmessage.Resource{aRecord("www.example.org.", [4]byte{192, 0, 2, 66})},

				Response:  true,
				Truncated: true,
			}
			out, _ := m.Pack()

			return out
		}},
		{"response too large", func(dnsmessage.Message) []byte { return make([]byte, 64*1024+1) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := rawServer(t, tt.respond)
			if got, err := doh.New(srv.URL, srv.Client()).Lookup(context.Background(), "www.example.org"); err == nil {
				t.Fatalf("want error, got %v", got)
			}
		})
	}
}

func TestLookupMatchesNameCaseInsensitively(t *testing.T) {
	t.Parallel()

	srv := dnsServer(t, map[dnsmessage.Type]reply{
		dnsmessage.TypeA: {records: []dnsmessage.Resource{aRecord("WWW.Example.ORG.", [4]byte{192, 0, 2, 1})}},
	})

	got, err := doh.New(srv.URL, srv.Client()).Lookup(context.Background(), "www.example.org")
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestLookupDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var hits int

	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	if _, err := doh.New(srv.URL, srv.Client()).Lookup(context.Background(), "www.example.org"); err == nil {
		t.Fatal("want error")
	}

	if hits != 0 {
		t.Fatalf("redirect target was hit %d times", hits)
	}
}
