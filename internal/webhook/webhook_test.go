package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	webhookapi "sigs.k8s.io/external-dns/provider/webhook/api"
)

const mediaType = webhookapi.MediaTypeFormatAndVersion

type fakeProvider struct {
	records []*endpoint.Endpoint
	err     error
	applied *plan.Changes
}

func (f *fakeProvider) Records(context.Context) ([]*endpoint.Endpoint, error) {
	return f.records, f.err
}

func (f *fakeProvider) ApplyChanges(_ context.Context, changes *plan.Changes) error {
	f.applied = changes

	return f.err
}

func (f *fakeProvider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	for _, ep := range endpoints {
		ep.RecordTTL = 42
	}

	return endpoints, f.err
}

func (f *fakeProvider) GetDomainFilter() endpoint.DomainFilterInterface {
	return endpoint.NewDomainFilter([]string{"example.com", ".example.com"})
}

func serve(t *testing.T, handler http.HandlerFunc, method, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)

	return rec
}

func TestNegotiate(t *testing.T) {
	h := New(&fakeProvider{})

	rec := serve(t, h.Negotiate, http.MethodGet, "", map[string]string{"Accept": mediaType})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != mediaType {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	var filter endpoint.DomainFilter
	if err := json.Unmarshal(rec.Body.Bytes(), &filter); err != nil {
		t.Fatalf("decoding domain filter: %v", err)
	}
	if !filter.Match("www.example.com") || filter.Match("example.org") {
		t.Errorf("round-tripped filter = %+v", filter)
	}
}

func TestMediaTypeValidation(t *testing.T) {
	h := New(&fakeProvider{})

	tests := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		headers map[string]string
		want    int
	}{
		{"records without accept", h.Records, http.MethodGet, nil, http.StatusNotAcceptable},
		{"records with wrong accept", h.Records, http.MethodGet,
			map[string]string{"Accept": "application/json"}, http.StatusUnsupportedMediaType},
		{"apply without content type", h.ApplyChanges, http.MethodPost, nil, http.StatusNotAcceptable},
		{"adjust without accept", h.AdjustEndpoints, http.MethodPost,
			map[string]string{"Content-Type": mediaType}, http.StatusNotAcceptable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := serve(t, tt.handler, tt.method, "{}", tt.headers); rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestRecords(t *testing.T) {
	p := &fakeProvider{records: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")}}
	h := New(p)

	rec := serve(t, h.Records, http.MethodGet, "", map[string]string{"Accept": mediaType})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []*endpoint.Endpoint
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].DNSName != "www.example.com" {
		t.Errorf("body = %s (%v)", rec.Body, err)
	}

	p.err = errors.New("boom")
	if rec := serve(t, h.Records, http.MethodGet, "", map[string]string{"Accept": mediaType}); rec.Code != http.StatusInternalServerError {
		t.Errorf("status on provider error = %d, want 500", rec.Code)
	}
}

func TestApplyChanges(t *testing.T) {
	p := &fakeProvider{}
	h := New(p)
	headers := map[string]string{"Content-Type": mediaType}

	rec := serve(t, h.ApplyChanges, http.MethodPost,
		`{"create":[{"dnsName":"www.example.com","recordType":"A","targets":["192.0.2.1"]}]}`, headers)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if p.applied == nil || len(p.applied.Create) != 1 || p.applied.Create[0].DNSName != "www.example.com" {
		t.Errorf("applied = %+v", p.applied)
	}

	if rec := serve(t, h.ApplyChanges, http.MethodPost, `{`, headers); rec.Code != http.StatusBadRequest {
		t.Errorf("status on bad JSON = %d, want 400", rec.Code)
	}

	p.err = errors.New("boom")
	if rec := serve(t, h.ApplyChanges, http.MethodPost, `{}`, headers); rec.Code != http.StatusInternalServerError {
		t.Errorf("status on provider error = %d, want 500", rec.Code)
	}
}

func TestAdjustEndpoints(t *testing.T) {
	h := New(&fakeProvider{})

	rec := serve(t, h.AdjustEndpoints, http.MethodPost,
		`[{"dnsName":"www.example.com","recordType":"A","targets":["192.0.2.1"]}]`,
		map[string]string{"Content-Type": mediaType, "Accept": mediaType})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []*endpoint.Endpoint
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].RecordTTL != 42 {
		t.Errorf("body = %s (%v)", rec.Body, err)
	}
}

func TestBodyTooLarge(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"in the value", `{"create":[]}`},
		{"after the value", `{}               `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProvider{}
			h := New(p)
			req := httptest.NewRequest(http.MethodPost, "/records", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", mediaType)
			rec := httptest.NewRecorder()
			req.Body = http.MaxBytesReader(rec, req.Body, 4)

			h.ApplyChanges(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge || p.applied != nil {
				t.Errorf("status = %d, applied = %v, want 413 and no apply", rec.Code, p.applied)
			}
		})
	}
}

func TestTrailingData(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"second value", `{"create":[]}{"delete":[]}`},
		{"garbage", `{"create":[]} x`},
		{"closing bracket", `{"create":[]}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProvider{}
			h := New(p)

			rec := serve(t, h.ApplyChanges, http.MethodPost, tt.body, map[string]string{"Content-Type": mediaType})
			if rec.Code != http.StatusBadRequest || p.applied != nil {
				t.Errorf("status = %d, applied = %v, want 400 and no apply", rec.Code, p.applied)
			}
		})
	}

	p := &fakeProvider{}
	rec := serve(t, New(p).ApplyChanges, http.MethodPost, "{\"create\":[]}\n", map[string]string{"Content-Type": mediaType})
	if rec.Code != http.StatusNoContent || p.applied == nil {
		t.Errorf("status with a trailing newline = %d, applied = %v, want 204 and an apply", rec.Code, p.applied)
	}
}
