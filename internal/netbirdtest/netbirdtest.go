// Package netbirdtest gives the unit and end-to-end tests an in-memory
// stand-in for the NetBird DNS zones API. It applies the record validation of
// the management server, so that tests find order and naming mistakes.
package netbirdtest

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netbirdio/netbird/shared/management/domain"
	"github.com/netbirdio/netbird/shared/management/http/api"
)

// Token is the personal access token the server accepts.
const Token = "nbp_test"

// Server is a running fake NetBird management API.
type Server struct {
	*httptest.Server

	mu         sync.Mutex
	zones      []api.Zone
	nextID     int
	calls      []string
	writeDelay time.Duration
	failWrites int
}

// NewServer starts a server that holds copies of zones. When the test
// finishes, the server closes.
func NewServer(t testing.TB, zones ...api.Zone) *Server {
	t.Helper()

	s := &Server{}
	for _, z := range zones {
		s.AddZone(z)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/dns/zones", s.listZones)
	mux.HandleFunc("POST /api/dns/zones/{zone}/records", s.createRecord)
	mux.HandleFunc("PUT /api/dns/zones/{zone}/records/{id}", s.updateRecord)
	mux.HandleFunc("DELETE /api/dns/zones/{zone}/records/{id}", s.deleteRecord)

	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.Method)
		delay := s.writeDelay
		fail := r.Method != http.MethodGet && s.failWrites > 0
		if fail {
			s.failWrites--
		}
		s.mu.Unlock()

		if r.Method != http.MethodGet {
			time.Sleep(delay)
		}
		if r.Header.Get("Authorization") != "Token "+Token {
			WriteError(w, http.StatusUnauthorized, "token invalid")

			return
		}
		if fail {
			WriteError(w, http.StatusUnprocessableEntity, "write rejected by the test")

			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)

	return s
}

// AddZone adds a copy of z, like a zone that a user creates in the dashboard.
func (s *Server) AddZone(z api.Zone) {
	s.mu.Lock()
	defer s.mu.Unlock()

	z.Records = slices.Clone(z.Records)
	s.zones = append(s.zones, z)
}

// SetWriteDelay makes every create, update, and delete take at least d.
func (s *Server) SetWriteDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.writeDelay = d
}

// FailWrites makes the next n creates, updates, and deletes fail with 422,
// without applying them. When NetBird rejects a change, it answers 422.
func (s *Server) FailWrites(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failWrites = n
}

// Calls returns the number of requests that used method. If method is empty,
// it returns the number of all requests.
func (s *Server) Calls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if method == "" {
		return len(s.calls)
	}

	n := 0
	for _, c := range s.calls {
		if c == method {
			n++
		}
	}

	return n
}

// Records returns every stored record as "zoneID name type content ttl",
// sorted.
func (s *Server) Records() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string
	for _, z := range s.zones {
		for _, r := range z.Records {
			out = append(out, strings.Join([]string{z.Id, r.Name, string(r.Type), r.Content, strconv.Itoa(r.Ttl)}, " "))
		}
	}
	slices.Sort(out)

	return out
}

func (s *Server) listZones(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	WriteJSON(w, s.zones)
}

func (s *Server) createRecord(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	z := s.zone(r.PathValue("zone"))
	if z == nil {
		WriteError(w, http.StatusNotFound, "zone not found")

		return
	}
	var rec api.DNSRecord
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		WriteError(w, http.StatusBadRequest, "couldn't parse JSON request")

		return
	}
	if status, msg := validate(z, rec); status != 0 {
		WriteError(w, status, msg)

		return
	}

	s.nextID++
	rec.Id = "rec-" + strconv.Itoa(s.nextID)
	z.Records = append(z.Records, rec)
	WriteJSON(w, rec)
}

func (s *Server) updateRecord(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	z := s.zone(r.PathValue("zone"))
	i := recordIndex(z, r.PathValue("id"))
	if i < 0 {
		WriteError(w, http.StatusNotFound, "record not found")

		return
	}
	var rec api.DNSRecord
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		WriteError(w, http.StatusBadRequest, "couldn't parse JSON request")

		return
	}
	rec.Id = z.Records[i].Id
	if status, msg := validate(z, rec); status != 0 {
		WriteError(w, status, msg)

		return
	}

	z.Records[i] = rec
	WriteJSON(w, rec)
}

func (s *Server) deleteRecord(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	z := s.zone(r.PathValue("zone"))
	i := recordIndex(z, r.PathValue("id"))
	if i < 0 {
		WriteError(w, http.StatusNotFound, "record not found")

		return
	}

	z.Records = slices.Delete(z.Records, i, i+1)
	WriteJSON(w, struct{}{})
}

func (s *Server) zone(id string) *api.Zone {
	for i := range s.zones {
		if s.zones[i].Id == id {
			return &s.zones[i]
		}
	}

	return nil
}

func recordIndex(z *api.Zone, id string) int {
	if z == nil {
		return -1
	}

	return slices.IndexFunc(z.Records, func(r api.DNSRecord) bool { return r.Id == id })
}

// validate mirrors Record.Validate and validateRecordConflicts in the
// management server.
func validate(z *api.Zone, rec api.DNSRecord) (int, string) {
	if msg := validateRecord(rec); msg != "" {
		return http.StatusUnprocessableEntity, msg
	}
	if rec.Name != z.Domain && !strings.HasSuffix(rec.Name, "."+z.Domain) {
		return http.StatusUnprocessableEntity, "record name does not belong to zone"
	}
	for _, existing := range z.Records {
		if existing.Id == rec.Id || existing.Name != rec.Name {
			continue
		}
		if existing.Type == rec.Type && existing.Content == rec.Content {
			return http.StatusConflict, "identical record already exists"
		}
		if rec.Type == api.DNSRecordTypeCNAME || existing.Type == api.DNSRecordTypeCNAME {
			return http.StatusUnprocessableEntity, "an A, AAAA, or CNAME record with this name already exists"
		}
	}

	return 0, ""
}

// validateRecord mirrors Record.Validate in the management server, which
// validates a record before its zone. It returns the error message, or "" for a
// valid record.
func validateRecord(rec api.DNSRecord) string {
	switch {
	case rec.Name == "":
		return "record name is required"
	case !domain.IsValidDomain(rec.Name):
		return "invalid record name format"
	}

	switch ip := net.ParseIP(rec.Content); rec.Type {
	case api.DNSRecordTypeA:
		if ip == nil || ip.To4() == nil {
			return "A record must be a valid IPv4 address"
		}
	case api.DNSRecordTypeAAAA:
		if ip == nil || ip.To4() != nil {
			return "AAAA record must be a valid IPv6 address"
		}
	case api.DNSRecordTypeCNAME:
		if !domain.IsValidDomainNoWildcard(rec.Content) {
			return "invalid CNAME target format"
		}
	default:
		return "invalid record type, must be A, AAAA, or CNAME"
	}

	if rec.Ttl < 0 {
		return "TTL cannot be negative"
	}

	return ""
}

// WriteJSON writes v as a 200 JSON response.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// If this fails, the test client gets a truncated body.
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes NetBird's JSON error envelope with status.
func WriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// If this fails, the test client gets a truncated body.
	_ = json.NewEncoder(w).Encode(map[string]any{"message": msg, "code": status})
}
