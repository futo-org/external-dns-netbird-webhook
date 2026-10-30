package netbird

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/config"
	"github.com/futo-org/external-dns-netbird-webhook/internal/metrics"
	"github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"
	"github.com/netbirdio/netbird/shared/management/http/util"
	extdnshttp "sigs.k8s.io/external-dns/pkg/http"
)

const (
	opListZones    = "list_zones"
	opCreateRecord = "create_record"
	opUpdateRecord = "update_record"
	opDeleteRecord = "delete_record"
)

// errorBodyLimit caps how much of an error response is kept as its message.
const errorBodyLimit = 512

// maxResponseBytes is the most that the client reads from a response body. It
// is a variable so that tests can make it smaller.
var maxResponseBytes int64 = 25 << 20

var errResponseTooLarge = errors.New("response too large")

// client calls the DNS zone endpoints through the NetBird REST SDK, which
// sends every request through a retryingClient.
type client struct {
	api *rest.Client
}

func newClient(cfg config.NetBird) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = cfg.ApplyWorkers

	return &client{api: rest.NewWithOptions(
		rest.WithManagementURL(strings.TrimRight(cfg.APIURL, "/")),
		rest.WithPAT(cfg.APIToken),
		rest.WithHttpClient(&retryingClient{
			httpc:        &http.Client{Transport: transport, Timeout: cfg.RequestTimeout},
			attempts:     cfg.RetryAttempts,
			initialDelay: cfg.RetryInitialDelay,
			maxDelay:     cfg.RetryMaxDelay,
		}),
	)}
}

// listZones returns every zone the token can read, with their records.
func (c *client) listZones(ctx context.Context) ([]api.Zone, error) {
	var zones []api.Zone
	err := instrument(ctx, opListZones, func(ctx context.Context) (err error) {
		zones, err = c.api.DNSZones.ListZones(ctx)

		return err
	})

	return zones, err
}

// createRecord creates r in zoneID. A 409 means an identical record already
// exists, which is the desired state.
func (c *client) createRecord(ctx context.Context, zoneID string, r api.DNSRecord) error {
	return instrument(ctx, opCreateRecord, func(ctx context.Context) error {
		_, err := c.api.DNSZones.CreateRecord(ctx, zoneID, requestFor(r))
		if hasStatus(err, http.StatusConflict) {
			slog.Debug("record already exists", "zone", zoneID, "name", r.Name, "type", r.Type, "content", r.Content)

			return nil
		}

		return err
	})
}

// updateRecord replaces the record r.Id in zoneID with r.
func (c *client) updateRecord(ctx context.Context, zoneID string, r api.DNSRecord) error {
	return instrument(ctx, opUpdateRecord, func(ctx context.Context) error {
		_, err := c.api.DNSZones.UpdateRecord(ctx, zoneID, r.Id, requestFor(r))

		return err
	})
}

// deleteRecord deletes recordID from zoneID. A 404 means the record is
// already gone, which is the desired state.
func (c *client) deleteRecord(ctx context.Context, zoneID, recordID string) error {
	return instrument(ctx, opDeleteRecord, func(ctx context.Context) error {
		err := c.api.DNSZones.DeleteRecord(ctx, zoneID, recordID)
		if hasStatus(err, http.StatusNotFound) {
			slog.Debug("record already deleted", "zone", zoneID, "id", recordID)

			return nil
		}

		return err
	})
}

func requestFor(r api.DNSRecord) api.DNSRecordRequest {
	return api.DNSRecordRequest{Name: r.Name, Type: r.Type, Content: r.Content, Ttl: r.Ttl}
}

type opKey struct{}

// instrument runs one SDK call under op. The requests of the call get op as
// their label for the retry and size metrics, which see only the
// *http.Request. When the call ends, retries included, instrument records its
// duration and result. It also adds the HTTP status to SDK API errors, because
// their message holds only the text from the server.
func instrument(ctx context.Context, op string, call func(context.Context) error) error {
	start := time.Now()
	err := call(context.WithValue(ctx, opKey{}, op))
	if apiErr, ok := errors.AsType[*rest.APIError](err); ok {
		err = fmt.Errorf("status %d: %w", apiErr.StatusCode, err)
	}
	metrics.Get().RecordAPICall(op, time.Since(start), err)

	return err
}

func hasStatus(err error, code int) bool {
	apiErr, ok := errors.AsType[*rest.APIError](err)

	return ok && apiErr.StatusCode == code
}

// retryingClient is the rest.HttpClient that the SDK sends through. It
// retries a 429 for any method. For idempotent methods, it also retries
// network errors and 5xx responses. It does not retry a POST for those errors,
// because NetBird can commit a POST before the failure.
type retryingClient struct {
	httpc        *http.Client
	attempts     int
	initialDelay time.Duration
	maxDelay     time.Duration
}

func (c *retryingClient) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	op, _ := ctx.Value(opKey{}).(string)
	m := metrics.Get()

	for attempt := 0; ; attempt++ {
		attemptReq := req
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("rewinding request body: %w", err)
			}
			attemptReq = req.Clone(ctx)
			attemptReq.Body = body
		}

		resp, err := c.httpc.Do(attemptReq)
		if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
			m.APIRateLimitsTotal.WithLabelValues(metrics.ProviderName, op).Inc()
		}
		// The SDK reads the body after Do returns, outside the retries. Read it
		// in the attempt, so that a failed or stalled read retries like a failed
		// request. A body over the limit does not change on a retry.
		if err == nil && resp.StatusCode < http.StatusMultipleChoices {
			if resp, err = bufferBody(resp, op); errors.Is(err, errResponseTooLarge) {
				return nil, err
			}
		}

		retry, wait := c.shouldRetry(req.Method, resp, err, attempt)
		if !retry || attempt == c.attempts-1 || ctx.Err() != nil {
			if resp != nil && resp.StatusCode >= http.StatusMultipleChoices {
				resp = errorResponse(resp)
			}

			return resp, err
		}

		status := "network"
		if resp != nil {
			status = strconv.Itoa(resp.StatusCode)
			extdnshttp.DrainAndClose(resp.Body)
		}
		m.APIRetriesTotal.WithLabelValues(metrics.ProviderName, op, status).Inc()
		slog.Debug("retrying netbird request", "op", op, "attempt", attempt+1, "status", status, "wait", wait)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (c *retryingClient) shouldRetry(method string, resp *http.Response, err error, attempt int) (bool, time.Duration) {
	idempotent := method != http.MethodPost

	if err != nil {
		return idempotent, c.backoff(attempt, 0)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return true, c.backoff(attempt, parseRetryAfter(resp.Header.Get("Retry-After")))
	case resp.StatusCode >= http.StatusInternalServerError && idempotent:
		return true, c.backoff(attempt, 0)
	}

	return false, 0
}

// backoff returns initial*2^attempt plus up to 50% jitter, never less than
// the server's Retry-After hint and never more than the configured maximum.
func (c *retryingClient) backoff(attempt int, hint time.Duration) time.Duration {
	base := c.initialDelay << attempt
	if base <= 0 || base > c.maxDelay {
		base = c.maxDelay
	}

	wait := base
	if half := int64(base) / 2; half > 0 {
		wait += time.Duration(rand.Int64N(half))
	}

	return min(max(wait, hint), c.maxDelay)
}

// parseRetryAfter understands both the delta-seconds and HTTP-date forms of
// Retry-After. Anything unparseable yields zero.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if secs, err := strconv.Atoi(value); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}

	return 0
}

// errorResponse replaces the body of a failed response with the JSON error
// envelope of NetBird, in a buffer. The SDK reports the status only through a
// *rest.APIError, and it builds that error from the envelope. Without this, an
// empty or HTML error page from a proxy loses the status. The SDK also never
// closes error bodies, so errorResponse closes the original body.
func errorResponse(resp *http.Response) *http.Response {
	body, err := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	extdnshttp.DrainAndClose(resp.Body)

	var envelope util.ErrorResponse
	if err != nil || json.Unmarshal(body, &envelope) != nil || envelope.Message == "" {
		envelope = util.ErrorResponse{Message: strings.TrimSpace(string(body)), Code: resp.StatusCode}
		if err != nil || envelope.Message == "" {
			envelope.Message = http.StatusText(resp.StatusCode)
		}
		// Marshalling a struct of a string and an int cannot fail.
		body, _ = json.Marshal(envelope)
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))

	return resp
}

// bufferBody reads the body of a successful response into memory, up to
// maxResponseBytes, and records its size. The SDK reads response bodies
// without a limit.
func bufferBody(resp *http.Response, op string) (*http.Response, error) {
	// One byte past the limit tells a body of exactly the limit from a larger one.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	// A body within the limit is already read to the end, so the connection
	// can be used again. To drain the rest of a larger body only delays the
	// error.
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("%w: it exceeds %d bytes", errResponseTooLarge, maxResponseBytes)
	}

	metrics.Get().APIResponseSizeBytes.WithLabelValues(metrics.ProviderName, op).Observe(float64(len(body)))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))

	return resp, nil
}
