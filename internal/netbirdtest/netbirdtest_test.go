package netbirdtest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/netbirdio/netbird/shared/management/http/api"
)

func TestCreateRecordValidation(t *testing.T) {
	tests := []struct {
		name   string
		record api.DNSRecord
		want   int
	}{
		{"valid A", api.DNSRecord{Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300}, http.StatusOK},
		{"valid wildcard name", api.DNSRecord{Name: "*.example.com", Type: "A", Content: "192.0.2.1"}, http.StatusOK},
		{"valid CNAME", api.DNSRecord{Name: "alias.example.com", Type: "CNAME", Content: "lb.example.net"}, http.StatusOK},
		{"missing name", api.DNSRecord{Type: "A", Content: "192.0.2.1"}, http.StatusUnprocessableEntity},
		{"name with trailing dot", api.DNSRecord{Name: "www.example.com.", Type: "A", Content: "192.0.2.1"}, http.StatusUnprocessableEntity},
		{"unsupported type", api.DNSRecord{Name: "www.example.com", Type: "TXT", Content: "hello"}, http.StatusUnprocessableEntity},
		{"A that is not an IP", api.DNSRecord{Name: "www.example.com", Type: "A", Content: "not-an-ip"}, http.StatusUnprocessableEntity},
		{"A with an IPv6 address", api.DNSRecord{Name: "www.example.com", Type: "A", Content: "2001:db8::1"}, http.StatusUnprocessableEntity},
		{"AAAA with an IPv4 address", api.DNSRecord{Name: "www.example.com", Type: "AAAA", Content: "192.0.2.1"}, http.StatusUnprocessableEntity},
		{"wildcard CNAME target", api.DNSRecord{Name: "alias.example.com", Type: "CNAME", Content: "*.example.net"}, http.StatusUnprocessableEntity},
		{"CNAME target with trailing dot", api.DNSRecord{Name: "alias.example.com", Type: "CNAME", Content: "lb.example.net."}, http.StatusUnprocessableEntity},
		{"negative TTL", api.DNSRecord{Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: -1}, http.StatusUnprocessableEntity},
		{"name outside the zone", api.DNSRecord{Name: "www.example.org", Type: "A", Content: "192.0.2.1"}, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer(t, api.Zone{Id: "z1", Domain: "example.com"})
			body, err := json.Marshal(tt.record)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL+"/api/dns/zones/z1/records", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Token "+Token)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}
