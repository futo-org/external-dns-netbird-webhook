package netbird

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/netbirdtest"
	"github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"
)

// statusSequence serves the given statuses in order and counts the requests.
// After that, it serves 200 with an empty zone list for GET, and an empty
// record for other methods.
func statusSequence(t *testing.T, statuses ...int) (*client, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1))
		if n <= len(statuses) {
			w.Header().Set("Retry-After", "0")
			netbirdtest.WriteError(w, statuses[n-1], http.StatusText(statuses[n-1]))

			return
		}
		if r.Method == http.MethodGet {
			netbirdtest.WriteJSON(w, []api.Zone{})

			return
		}
		netbirdtest.WriteJSON(w, api.DNSRecord{})
	}))
	t.Cleanup(srv.Close)

	return newClient(testConfig(srv.URL)), &hits
}

func TestRetries(t *testing.T) {
	tests := []struct {
		name     string
		statuses []int
		call     func(*client) error
		wantErr  bool
		wantHits int32
	}{
		{
			name:     "GET retried on 503",
			statuses: []int{503, 503},
			call:     func(c *client) error { _, err := c.listZones(t.Context()); return err },
			wantHits: 3,
		},
		{
			name:     "GET gives up after the configured attempts",
			statuses: []int{503, 503, 503},
			call:     func(c *client) error { _, err := c.listZones(t.Context()); return err },
			wantErr:  true,
			wantHits: 3,
		},
		{
			name:     "POST not retried on 503",
			statuses: []int{503},
			call:     func(c *client) error { return c.createRecord(t.Context(), "z", api.DNSRecord{}) },
			wantErr:  true,
			wantHits: 1,
		},
		{
			name:     "POST retried on 429",
			statuses: []int{429},
			call:     func(c *client) error { return c.createRecord(t.Context(), "z", api.DNSRecord{}) },
			wantHits: 2,
		},
		{
			name:     "PUT retried on 502",
			statuses: []int{502},
			call:     func(c *client) error { return c.updateRecord(t.Context(), "z", api.DNSRecord{Id: "r"}) },
			wantHits: 2,
		},
		{
			name:     "4xx not retried",
			statuses: []int{422},
			call:     func(c *client) error { return c.updateRecord(t.Context(), "z", api.DNSRecord{Id: "r"}) },
			wantErr:  true,
			wantHits: 1,
		},
		{
			name:     "POST 409 means the record already exists",
			statuses: []int{409},
			call:     func(c *client) error { return c.createRecord(t.Context(), "z", api.DNSRecord{}) },
			wantHits: 1,
		},
		{
			name:     "DELETE 404 means the record is already gone",
			statuses: []int{404},
			call:     func(c *client) error { return c.deleteRecord(t.Context(), "z", "r") },
			wantHits: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, hits := statusSequence(t, tt.statuses...)

			err := tt.call(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := hits.Load(); got != tt.wantHits {
				t.Errorf("hits = %d, want %d", got, tt.wantHits)
			}
		})
	}
}

func TestRetriesNetworkErrorsForIdempotentMethods(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)

				return
			}
			_ = conn.Close()

			return
		}
		netbirdtest.WriteJSON(w, []api.Zone{})
	}))
	t.Cleanup(srv.Close)
	c := newClient(testConfig(srv.URL))

	if _, err := c.listZones(t.Context()); err != nil {
		t.Fatalf("listZones: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("hits = %d, want 2", got)
	}

	hits.Store(0)
	if err := c.createRecord(t.Context(), "z", api.DNSRecord{}); err == nil {
		t.Fatal("createRecord succeeded, want the connection error")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("POST hits = %d, want 1", got)
	}
}

func TestRequestShape(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		gotBody = string(body)
		netbirdtest.WriteJSON(w, api.DNSRecord{})
	}))
	t.Cleanup(srv.Close)
	c := newClient(testConfig(srv.URL + "/"))

	err := c.updateRecord(t.Context(), "z1", api.DNSRecord{
		Id: "r1", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 60,
	})
	if err != nil {
		t.Fatalf("updateRecord: %v", err)
	}

	if gotMethod != http.MethodPut || gotPath != "/api/dns/zones/z1/records/r1" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Token "+netbirdtest.Token {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if want := `{"content":"192.0.2.1","name":"www.example.com","ttl":60,"type":"A"}`; strings.TrimSpace(gotBody) != want {
		t.Errorf("body = %s, want %s", gotBody, want)
	}
}

func TestAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		netbirdtest.WriteError(w, http.StatusUnprocessableEntity, "record name does not belong to zone")
	}))
	t.Cleanup(srv.Close)

	_, err := newClient(testConfig(srv.URL)).listZones(t.Context())
	if want := "status 422: record name does not belong to zone"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if apiErr, ok := errors.AsType[*rest.APIError](err); !ok || apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("err = %#v, want a wrapped *rest.APIError with status 422", err)
	}
}

func TestListZonesRejectsOversizedResponse(t *testing.T) {
	old := maxResponseBytes
	maxResponseBytes = 16
	t.Cleanup(func() { maxResponseBytes = old })

	tests := []struct {
		name  string
		stall bool
	}{
		{"complete body", false},
		{"body that stalls after the limit", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				netbirdtest.WriteJSON(w, []api.Zone{{Id: "a-long-zone-id", Domain: "example.com"}})
				if tt.stall {
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Errorf("flush: %v", err)
					}
					<-release
				}
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { close(release) })

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			start := time.Now()
			_, err := newClient(testConfig(srv.URL)).listZones(ctx)
			if err == nil || !strings.Contains(err.Error(), "exceeds 16 bytes") {
				t.Fatalf("err = %v, want size limit error", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("listZones took %s, want the size error without waiting for the rest of the body", elapsed)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	c := &retryingClient{initialDelay: 100 * time.Millisecond, maxDelay: time.Second}

	for attempt, base := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond} {
		if got := c.backoff(attempt, 0); got < base || got >= base*3/2 {
			t.Errorf("backoff(%d) = %s, want in [%s, %s)", attempt, got, base, base*3/2)
		}
	}
	if got := c.backoff(10, 0); got != time.Second {
		t.Errorf("backoff(10) = %s, want the 1s cap", got)
	}
	if got := c.backoff(0, 700*time.Millisecond); got != 700*time.Millisecond {
		t.Errorf("backoff with hint = %s, want the 700ms hint", got)
	}
	if got := c.backoff(0, time.Minute); got != time.Second {
		t.Errorf("backoff with huge hint = %s, want the 1s cap", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	tests := map[string]struct {
		value   string
		atLeast time.Duration
		atMost  time.Duration
	}{
		"seconds":      {"5", 5 * time.Second, 5 * time.Second},
		"http date":    {future, 59 * time.Minute, time.Hour},
		"past date":    {"Mon, 02 Jan 2006 15:04:05 GMT", 0, 0},
		"garbage":      {"soon", 0, 0},
		"empty":        {"", 0, 0},
		"negative":     {"-1", 0, 0},
		"surrounding":  {" 2 ", 2 * time.Second, 2 * time.Second},
		"fractional s": {"1.5", 0, 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := parseRetryAfter(tt.value); got < tt.atLeast || got > tt.atMost {
				t.Errorf("parseRetryAfter(%q) = %s, want in [%s, %s]", tt.value, got, tt.atLeast, tt.atMost)
			}
		})
	}
}

type stubTransport struct {
	status int
	body   string
	closed atomic.Bool
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: s.status,
		Header:     http.Header{},
		Body:       &trackedBody{Reader: strings.NewReader(s.body), closed: &s.closed},
		Request:    req,
	}, nil
}

type trackedBody struct {
	io.Reader

	closed *atomic.Bool
}

func (b *trackedBody) Close() error {
	b.closed.Store(true)

	return nil
}

func TestErrorResponses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		call    func(*client) error
		wantErr string
	}{
		{
			name:   "HTML 404 on delete still means already gone",
			status: http.StatusNotFound,
			body:   "<html><body>404 Not Found</body></html>",
			call:   func(c *client) error { return c.deleteRecord(t.Context(), "z", "r") },
		},
		{
			name:    "empty error body keeps the status",
			status:  http.StatusBadGateway,
			call:    func(c *client) error { _, err := c.listZones(t.Context()); return err },
			wantErr: "status 502: Bad Gateway",
		},
		{
			name:    "HTML error body becomes the message",
			status:  http.StatusForbidden,
			body:    "<h1>denied</h1>\n",
			call:    func(c *client) error { _, err := c.listZones(t.Context()); return err },
			wantErr: "status 403: <h1>denied</h1>",
		},
		{
			name:    "JSON error body is kept",
			status:  http.StatusUnprocessableEntity,
			body:    `{"message":"invalid CNAME target format","code":422}`,
			call:    func(c *client) error { return c.createRecord(t.Context(), "z", api.DNSRecord{}) },
			wantErr: "status 422: invalid CNAME target format",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubTransport{status: tt.status, body: tt.body}
			c := &client{api: rest.NewWithOptions(
				rest.WithManagementURL("http://netbird.test"),
				rest.WithPAT(netbirdtest.Token),
				rest.WithHttpClient(&retryingClient{httpc: &http.Client{Transport: stub}, attempts: 1}),
			)}

			err := tt.call(c)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if !stub.closed.Load() {
				t.Error("error response body was not closed")
			}
		})
	}
}

func TestRetriesStalledBodyRead(t *testing.T) {
	tests := []struct {
		name     string
		call     func(*client) error
		wantErr  bool
		wantHits int32
	}{
		{
			name:     "GET retries a body that stalls past the attempt timeout",
			call:     func(c *client) error { _, err := c.listZones(t.Context()); return err },
			wantHits: 2,
		},
		{
			name:     "POST does not retry, because the create can have landed",
			call:     func(c *client) error { return c.createRecord(t.Context(), "z", api.DNSRecord{}) },
			wantErr:  true,
			wantHits: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) == 1 {
					w.WriteHeader(http.StatusOK)
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Errorf("flushing headers: %v", err)
					}
					select {
					case <-r.Context().Done():
					case <-time.After(time.Second):
					}

					return
				}
				if r.Method == http.MethodGet {
					netbirdtest.WriteJSON(w, []api.Zone{})

					return
				}
				netbirdtest.WriteJSON(w, api.DNSRecord{})
			}))
			t.Cleanup(srv.Close)
			cfg := testConfig(srv.URL)
			cfg.RequestTimeout = 100 * time.Millisecond

			err := tt.call(newClient(cfg))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := hits.Load(); got != tt.wantHits {
				t.Errorf("hits = %d, want %d", got, tt.wantHits)
			}
		})
	}
}
