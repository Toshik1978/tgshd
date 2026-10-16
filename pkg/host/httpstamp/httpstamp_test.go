package httpstamp_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/host/httpstamp"
)

// countingTransport records that the injected transport carried the request.
type countingTransport struct {
	next  http.RoundTripper
	calls atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)

	return c.next.RoundTrip(r)
}

func TestFetch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		headerName string
		headerVal  string
		status     int
		body       string
	}{
		{
			name:       "ok with header",
			headerName: "X-Token",
			headerVal:  "s3cret",
			status:     http.StatusOK,
			body:       "1767225600\n",
		},
		{name: "no header", status: http.StatusOK, body: "1767225600"},
		{name: "non-2xx is reported, not judged", status: http.StatusServiceUnavailable, body: "down"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotMethod, gotHeader string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotHeader = r.Header.Get("X-Token")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			tr := &countingTransport{next: srv.Client().Transport}
			status, body, err := httpstamp.New(tr).Fetch(context.Background(), srv.URL, tt.headerName, tt.headerVal)
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			if status != tt.status || string(body) != tt.body {
				t.Errorf("Fetch() = %d, %q, want %d, %q", status, body, tt.status, tt.body)
			}
			if gotMethod != http.MethodGet || gotHeader != tt.headerVal {
				t.Errorf("server saw %s with header %q, want GET with %q", gotMethod, gotHeader, tt.headerVal)
			}
			if tr.calls.Load() != 1 {
				t.Errorf("injected transport used %d times, want 1", tr.calls.Load())
			}
		})
	}
}

func TestFetchErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	if _, _, err := httpstamp.New(nil).Fetch(context.Background(), url, "", ""); err == nil {
		t.Error("Fetch() against a closed server: want error, got nil")
	}
	if _, _, err := httpstamp.New(nil).Fetch(context.Background(), "://bad", "", ""); err == nil {
		t.Error("Fetch() with a malformed URL: want error, got nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := httpstamp.New(nil).Fetch(ctx, "http://192.0.2.1/", "", ""); err == nil {
		t.Error("Fetch() with a cancelled context: want error, got nil")
	}
}

func TestNoRedirect(t *testing.T) {
	t.Parallel()

	var targetHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			targetHits.Add(1)
			_, _ = w.Write([]byte("1767225600"))

			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer srv.Close()

	status, _, err := httpstamp.New(srv.Client().Transport).
		Fetch(context.Background(), srv.URL+"/start", "X-Token", "s3cret")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if status != http.StatusFound {
		t.Errorf("status = %d, want the redirect status %d", status, http.StatusFound)
	}
	if targetHits.Load() != 0 {
		t.Errorf("redirect target was requested %d times, want 0", targetHits.Load())
	}
}

func TestBodyCap(t *testing.T) {
	t.Parallel()

	big := bytes.Repeat([]byte("7"), 10*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	status, body, err := httpstamp.New(srv.Client().Transport).Fetch(context.Background(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if status != http.StatusOK || len(body) != 4096 {
		t.Errorf("Fetch() = %d, %d bytes, want 200 and exactly 4096 bytes", status, len(body))
	}
}

// TestFetchErrorOmitsURL proves a failed fetch does not repeat the url, whose
// query may hold a secret.
func TestFetchErrorOmitsURL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	stampURL := srv.URL + "/stamp?key=SECRET-q-123"
	srv.Close()

	_, _, err := httpstamp.New(nil).Fetch(context.Background(), stampURL, "", "")
	if err == nil {
		t.Fatal("Fetch to a closed server succeeded")
	}
	if strings.Contains(err.Error(), "SECRET-q-123") {
		t.Fatalf("error repeats the url: %v", err)
	}
}
