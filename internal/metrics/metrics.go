// Package metrics holds the service's Prometheus metrics (ARCHITECTURE.md
// §3.8). It is the only package that imports the Prometheus client: the rest
// of the code calls the small Record functions below, and Handler serves
// GET /metrics.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	expensecore "auto-itemizer/internal/expense/core"
	receiptcore "auto-itemizer/internal/receipt/core"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// The metrics live in the client's default registry, which also holds Go's
// own go_* and process_* metrics. Labels are route patterns and fixed codes,
// never ids, so the number of series stays small.
var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "HTTP requests by route pattern and status code.",
	}, []string{"route", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Time taken to answer an HTTP request, by route pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route"})

	receiptOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "receipt_outcomes_total",
		Help: "Processed receipts by outcome: COMPLETE, NEEDS_REVIEW, or the code of the guard that failed.",
	}, []string{"outcome"})

	mockOCRFallbacks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mock_ocr_fallbacks_total",
		Help: "Uploads whose file name matched no mock OCR fixture, so the fallback text was used.",
	})

	patchesRefused = promauto.NewCounter(prometheus.CounterOpts{
		Name: "patches_refused_total",
		Help: "PATCH requests refused because the items plus taxes don't equal the total (409 ITEMS_DO_NOT_RECONCILE).",
	})

	writeConflicts = promauto.NewCounter(prometheus.CounterOpts{
		Name: "write_conflicts_total",
		Help: "Writes refused because another request changed the row first (409 CONFLICT).",
	})

	internalErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "internal_errors_total",
		Help: "Unexpected errors answered with 500 INTERNAL_ERROR.",
	})

	panics = promauto.NewCounter(prometheus.CounterOpts{
		Name: "panics_total",
		Help: "Handler panics turned into 500 INTERNAL_ERROR.",
	})
)

func init() {
	// Every outcome shows in /metrics from the start, at 0, so a graph or an
	// alert never waits for the first failure of a kind.
	for _, outcome := range []string{
		expensecore.StatusComplete,
		expensecore.StatusNeedsReview,
		receiptcore.CodeOCRFailed,
		receiptcore.CodeOCRUnreadable,
		receiptcore.CodeNotAReceipt,
		receiptcore.CodeHeaderIncomplete,
		receiptcore.CodeInvalidReceiptValues,
	} {
		receiptOutcomes.WithLabelValues(outcome)
	}
}

// RecordRequest records one answered HTTP request. route is the router's
// pattern, such as "POST /receipts/{id}/process".
func RecordRequest(route string, status int, duration time.Duration) {
	requests.WithLabelValues(route, strconv.Itoa(status)).Inc()
	requestDuration.WithLabelValues(route).Observe(duration.Seconds())
}

// RecordReceiptOutcome records how a processed receipt ended: its
// itemization status, or the code of the guard it failed.
func RecordReceiptOutcome(outcome string) {
	receiptOutcomes.WithLabelValues(outcome).Inc()
}

// RecordMockOCRFallback records an upload whose name matched no fixture.
func RecordMockOCRFallback() { mockOCRFallbacks.Inc() }

// RecordPatchRefused records a PATCH refused with 409 ITEMS_DO_NOT_RECONCILE.
func RecordPatchRefused() { patchesRefused.Inc() }

// RecordWriteConflict records a write refused with 409 CONFLICT.
func RecordWriteConflict() { writeConflicts.Inc() }

// RecordInternalError records an unexpected error answered with 500.
func RecordInternalError() { internalErrors.Inc() }

// RecordPanic records a handler panic turned into a 500.
func RecordPanic() { panics.Inc() }

// Handler serves every metric in the Prometheus text format.
func Handler() http.Handler { return promhttp.Handler() }
