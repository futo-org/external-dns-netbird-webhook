// Package webhook adapts an external-dns provider.Provider to the ExternalDNS
// webhook HTTP protocol.
package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/futo-org/external-dns-netbird-webhook/internal/metrics"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
	webhookapi "sigs.k8s.io/external-dns/provider/webhook/api"
)

const (
	headerAccept      = "Accept"
	headerContentType = "Content-Type"
	headerVary        = "Vary"
	contentTypePlain  = "text/plain"
)

// Webhook serves the external-dns webhook endpoints for a provider.
type Webhook struct {
	provider provider.Provider
}

// New returns a Webhook backed by p.
func New(p provider.Provider) *Webhook {
	return &Webhook{provider: p}
}

// Negotiate handles GET / and returns the provider's domain filter.
func (h *Webhook) Negotiate(w http.ResponseWriter, r *http.Request) {
	metrics.Get().NegotiateTotal.WithLabelValues(metrics.ProviderName).Inc()

	if !checkMediaType(w, r, headerAccept) {
		return
	}

	writeJSON(w, r, h.provider.GetDomainFilter())
}

// Records handles GET /records.
func (h *Webhook) Records(w http.ResponseWriter, r *http.Request) {
	if !checkMediaType(w, r, headerAccept) {
		return
	}

	records, err := h.provider.Records(r.Context())
	if err != nil {
		writeInternalError(w, r, "getting records", err)

		return
	}

	writeJSON(w, r, records)
}

// ApplyChanges handles POST /records.
func (h *Webhook) ApplyChanges(w http.ResponseWriter, r *http.Request) {
	if !checkMediaType(w, r, headerContentType) {
		return
	}

	changes, ok := decodeBody[plan.Changes](w, r)
	if !ok {
		return
	}

	requestLog(r).Debug("applying changes",
		"create", len(changes.Create),
		"update_old", len(changes.UpdateOld),
		"update_new", len(changes.UpdateNew),
		"delete", len(changes.Delete),
	)

	if err := h.provider.ApplyChanges(r.Context(), &changes); err != nil {
		writeInternalError(w, r, "applying changes", err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AdjustEndpoints handles POST /adjustendpoints.
func (h *Webhook) AdjustEndpoints(w http.ResponseWriter, r *http.Request) {
	metrics.Get().AdjustEndpointsTotal.WithLabelValues(metrics.ProviderName).Inc()

	if !checkMediaType(w, r, headerContentType) || !checkMediaType(w, r, headerAccept) {
		return
	}

	endpoints, ok := decodeBody[[]*endpoint.Endpoint](w, r)
	if !ok {
		return
	}

	endpoints, err := h.provider.AdjustEndpoints(endpoints)
	if err != nil {
		writeInternalError(w, r, "adjusting endpoints", err)

		return
	}

	writeJSON(w, r, endpoints)
}

// checkMediaType makes sure that header carries the webhook media type. If it
// does not, checkMediaType writes the error response.
func checkMediaType(w http.ResponseWriter, r *http.Request, header string) bool {
	value := r.Header.Get(header)
	if value == webhookapi.MediaTypeFormatAndVersion {
		return true
	}

	metrics.Get().HTTPValidationErrorsTotal.WithLabelValues(metrics.ProviderName, strings.ToLower(header)).Inc()
	if value == "" {
		writePlain(w, r, http.StatusNotAcceptable, fmt.Sprintf("client must provide a %s header", header))
	} else {
		writePlain(w, r, http.StatusUnsupportedMediaType,
			fmt.Sprintf("unsupported %s %q, supported media type is %s", header, value, webhookapi.MediaTypeFormatAndVersion))
	}

	return false
}

// decodeBody decodes the JSON request body into T. If the body is over the
// limit, it writes 413. For any other decode failure, it writes 400.
func decodeBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	err := json.NewDecoder(r.Body).Decode(&v)
	if err == nil {
		return v, true
	}

	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		writePlain(w, r, http.StatusRequestEntityTooLarge, "request body too large")

		return v, false
	}

	metrics.Get().HTTPJSONErrorsTotal.WithLabelValues(metrics.ProviderName, r.URL.Path).Inc()
	writePlain(w, r, http.StatusBadRequest, fmt.Sprintf("error decoding request body: %v", err))

	return v, false
}

// writeJSON encodes v with the webhook media type. If the encode fails,
// writeJSON only logs it, because the 200 status line is already sent.
func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set(headerContentType, webhookapi.MediaTypeFormatAndVersion)
	w.Header().Set(headerVary, headerContentType)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		requestLog(r).Error("encoding response", "error", err)
	}
}

// writeInternalError logs a provider failure and answers 500, which
// external-dns retries on its next sync. The cause stays in the log.
func writeInternalError(w http.ResponseWriter, r *http.Request, action string, err error) {
	requestLog(r).Error(action, "error", err)
	writePlain(w, r, http.StatusInternalServerError, "")
}

func writePlain(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.Header().Set(headerContentType, contentTypePlain)
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, msg); err != nil {
		requestLog(r).Error("writing error response", "error", err)
	}
}

func requestLog(r *http.Request) *slog.Logger {
	return slog.With("method", r.Method, "path", r.URL.Path)
}
