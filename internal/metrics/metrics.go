package metrics

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	namespace    = "externaldns_webhook"
	ProviderName = "netbird"

	sizeBucketCount = 8

	labelProvider   = "provider"
	labelEndpoint   = "endpoint"
	labelMethod     = "method"
	labelOperation  = "operation"
	labelRecordType = "record_type"
)

// Metrics holds all Prometheus metrics for the webhook.
type Metrics struct {
	HTTPRequestsTotal         *prometheus.CounterVec
	HTTPRequestDuration       *prometheus.HistogramVec
	HTTPRequestsInFlight      *prometheus.GaugeVec
	HTTPResponseSizeBytes     *prometheus.HistogramVec
	HTTPValidationErrorsTotal *prometheus.CounterVec
	HTTPJSONErrorsTotal       *prometheus.CounterVec
	PanicsTotal               *prometheus.CounterVec

	RecordsTotal             *prometheus.GaugeVec
	ChangesTotal             *prometheus.CounterVec
	ChangesByTypeTotal       *prometheus.CounterVec
	IgnoredCNAMETargetsTotal *prometheus.CounterVec
	BatchSize                *prometheus.HistogramVec

	AdjustEndpointsTotal *prometheus.CounterVec
	NegotiateTotal       *prometheus.CounterVec

	APIErrorsTotal       *prometheus.CounterVec
	APIDuration          *prometheus.HistogramVec
	APIResponseSizeBytes *prometheus.HistogramVec
	APIRetriesTotal      *prometheus.CounterVec
	APIRateLimitsTotal   *prometheus.CounterVec

	ConsecutiveErrors    *prometheus.GaugeVec
	LastSuccessTimestamp *prometheus.GaugeVec

	Info *prometheus.GaugeVec
}

var (
	initOnce sync.Once
	instance *Metrics
)

// New initializes the package-wide metrics instance against the default
// Prometheus registerer. Only the first call has an effect.
func New(version string) *Metrics {
	initOnce.Do(func() {
		instance = build(promauto.With(prometheus.DefaultRegisterer), version)
	})

	return instance
}

// Get returns the singleton metrics instance. If New did not run before, Get
// initializes the instance with the version "unknown".
func Get() *Metrics {
	return New("unknown")
}

func build(f promauto.Factory, version string) *Metrics {
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return f.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
	}
	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		return f.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
	}
	histogram := func(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		return f.NewHistogramVec(
			prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help, Buckets: buckets}, labels)
	}
	sizeBuckets := prometheus.ExponentialBuckets(100, 10, sizeBucketCount)

	m := &Metrics{
		HTTPRequestsTotal: counter("http_requests_total", "Total number of HTTP requests",
			labelProvider, labelMethod, labelEndpoint, "status_code"),
		HTTPRequestDuration: histogram("http_request_duration_seconds", "HTTP request duration in seconds",
			prometheus.DefBuckets, labelProvider, labelMethod, labelEndpoint),
		HTTPRequestsInFlight: gauge("http_requests_in_flight", "Number of HTTP requests currently being processed",
			labelProvider),
		HTTPResponseSizeBytes: histogram("http_response_size_bytes", "HTTP response size in bytes",
			sizeBuckets, labelProvider, labelMethod, labelEndpoint),
		HTTPValidationErrorsTotal: counter("http_validation_errors_total", "Total number of HTTP header validation errors",
			labelProvider, "header_type"),
		HTTPJSONErrorsTotal: counter("http_json_errors_total", "Total number of JSON decoding errors",
			labelProvider, labelEndpoint),
		PanicsTotal: counter("http_handler_panics_total", "Total number of panics caught by the HTTP recovery middleware",
			labelProvider, labelEndpoint),

		RecordsTotal: gauge("records", "Current number of DNS records by type",
			labelProvider, labelRecordType),
		ChangesTotal: counter("changes_total", "Total number of DNS changes",
			labelProvider, labelOperation),
		ChangesByTypeTotal: counter("changes_by_type_total", "Total number of DNS changes by record type",
			labelProvider, labelOperation, labelRecordType),
		IgnoredCNAMETargetsTotal: counter("ignored_cname_targets_total",
			"Total number of ignored CNAME targets (only the first target is used)", labelProvider),
		BatchSize: histogram("batch_size", "Size of change batches",
			prometheus.ExponentialBuckets(1, 2, 10), labelProvider, labelOperation),

		AdjustEndpointsTotal: counter("adjust_endpoints_total", "Total number of adjust endpoints calls", labelProvider),
		NegotiateTotal:       counter("negotiate_total", "Total number of negotiate calls", labelProvider),

		APIErrorsTotal: counter("netbird_api_errors_total", "Total number of NetBird API errors",
			labelProvider, labelOperation),
		APIDuration: histogram("netbird_api_duration_seconds", "NetBird API request duration in seconds",
			prometheus.DefBuckets, labelProvider, labelOperation),
		APIResponseSizeBytes: histogram("netbird_api_response_size_bytes", "NetBird API response size in bytes",
			sizeBuckets, labelProvider, labelOperation),
		APIRetriesTotal: counter("netbird_api_retries_total",
			"Total number of NetBird API retries triggered by network errors, 5xx, or 429 responses",
			labelProvider, labelOperation, "status"),
		APIRateLimitsTotal: counter("netbird_api_rate_limits_total",
			"Total number of HTTP 429 rate-limit responses received from NetBird", labelProvider, labelOperation),

		ConsecutiveErrors: gauge("consecutive_errors", "Number of consecutive failed provider operations",
			labelProvider),
		LastSuccessTimestamp: gauge("last_success_timestamp", "Timestamp of the last successful provider operation",
			labelProvider),

		Info: gauge("info", "Information about the webhook instance", "version", labelProvider),
	}

	m.Info.WithLabelValues(version, ProviderName).Set(1)

	return m
}

// RecordHTTPRequest records metrics for one served HTTP request.
func (m *Metrics) RecordHTTPRequest(method, endpoint string, statusCode int, duration time.Duration, responseSize int) {
	m.HTTPRequestsTotal.WithLabelValues(ProviderName, method, endpoint, strconv.Itoa(statusCode)).Inc()
	m.HTTPRequestDuration.WithLabelValues(ProviderName, method, endpoint).Observe(duration.Seconds())
	if responseSize > 0 {
		m.HTTPResponseSizeBytes.WithLabelValues(ProviderName, method, endpoint).Observe(float64(responseSize))
	}
}

// RecordAPICall records metrics for one NetBird API call, including retries.
func (m *Metrics) RecordAPICall(operation string, duration time.Duration, err error) {
	m.APIDuration.WithLabelValues(ProviderName, operation).Observe(duration.Seconds())
	if err != nil {
		m.APIErrorsTotal.WithLabelValues(ProviderName, operation).Inc()
	}
}

// RecordOperation records the result of one top-level provider operation
// (Records or ApplyChanges). It counts per operation, not per API call,
// because ApplyChanges makes calls at the same time. Per call, the success of
// one worker resets the consecutive-error gauge while other workers fail.
func (m *Metrics) RecordOperation(err error) {
	if err != nil {
		m.ConsecutiveErrors.WithLabelValues(ProviderName).Inc()

		return
	}
	m.ConsecutiveErrors.WithLabelValues(ProviderName).Set(0)
	m.LastSuccessTimestamp.WithLabelValues(ProviderName).Set(float64(time.Now().Unix()))
}

// UpdateRecordsByType sets the current record count for recordType.
func (m *Metrics) UpdateRecordsByType(recordType string, count int) {
	m.RecordsTotal.WithLabelValues(ProviderName, recordType).Set(float64(count))
}

// RecordChange counts one applied DNS change.
func (m *Metrics) RecordChange(operation, recordType string) {
	m.ChangesTotal.WithLabelValues(ProviderName, operation).Inc()
	m.ChangesByTypeTotal.WithLabelValues(ProviderName, operation, recordType).Inc()
}
