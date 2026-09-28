package metrics

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scrape returns the /metrics text.
func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	return rec.Body.String()
}

// value returns one series' value from the /metrics text, or 0 when the
// series isn't there yet.
func value(t *testing.T, text, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, series+" "); ok {
			v, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			return v
		}
	}
	return 0
}

// Each Record function shows up in /metrics under its name and labels.
// The counters are process-wide, so the test compares before and after.
func TestRecordFunctions(t *testing.T) {
	before := scrape(t)
	RecordRequest("GET /health", http.StatusOK, 3*time.Millisecond)
	RecordReceiptOutcome("NOT_A_RECEIPT")
	RecordMockOCRFallback()
	RecordPatchRefused()
	RecordWriteConflict()
	RecordInternalError()
	RecordPanic()
	after := scrape(t)

	for _, series := range []string{
		`http_requests_total{route="GET /health",status="200"}`,
		`http_request_duration_seconds_count{route="GET /health"}`,
		`receipt_outcomes_total{outcome="NOT_A_RECEIPT"}`,
		"mock_ocr_fallbacks_total",
		"patches_refused_total",
		"write_conflicts_total",
		"internal_errors_total",
		"panics_total",
	} {
		if got := value(t, after, series) - value(t, before, series); got != 1 {
			t.Errorf("%s went up by %v, want 1", series, got)
		}
	}
}

// Every outcome is listed from the start, so graphs and alerts see a 0
// before the first failure of that kind.
func TestOutcomesStartAtZero(t *testing.T) {
	text := scrape(t)
	for _, outcome := range []string{"COMPLETE", "NEEDS_REVIEW", "OCR_FAILED", "OCR_UNREADABLE",
		"NOT_A_RECEIPT", "HEADER_INCOMPLETE", "INVALID_RECEIPT_VALUES"} {
		if !strings.Contains(text, `receipt_outcomes_total{outcome="`+outcome+`"}`) {
			t.Errorf("receipt_outcomes_total has no %s series", outcome)
		}
	}
}
