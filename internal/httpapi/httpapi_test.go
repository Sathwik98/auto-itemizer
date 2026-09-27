package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/expense"
	"auto-itemizer/internal/fileupload"
	"auto-itemizer/internal/ocr"
	"auto-itemizer/internal/receipt"
	"auto-itemizer/internal/receipt/parser"

	"github.com/shopspring/decimal"
)

var ctx = context.Background()

func fixturePath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "fixtures"}, parts...)...)
}

// newAPI wires the whole stack the way main.go will: a temporary database and
// storage folder, real services, and the given OCR provider (nil means the
// mock OCR over the fixtures).
func newAPI(t *testing.T, provider ocr.Provider) http.Handler {
	t.Helper()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	files, err := fileupload.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if provider == nil {
		provider, err = ocr.NewMockProvider(
			[]string{fixturePath("task-a"), fixturePath("mock-ocr")},
			fixturePath("task-a", "receipt-clean.txt"))
		if err != nil {
			t.Fatal(err)
		}
	}
	expenses := expense.New(d)
	names, err := expenses.TaxNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	receipts := receipt.New(d, files, ocr.New(provider, time.Second), expenses, parser.New(names))
	expenses.SetParsedReceiptSource(receipts)
	return NewHandler(receipts, expenses)
}

// response is a decoded JSON response.
type response struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (r response) str(key string) string {
	s, _ := r.body[key].(string)
	return s
}

func send(t *testing.T, h http.Handler, method, path, contentType string, body io.Reader) response {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	res := response{status: rec.Code, header: rec.Header(), raw: rec.Body.String()}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &res.body); err != nil {
			t.Fatalf("%s %s: response is not a JSON object: %q", method, path, rec.Body.String())
		}
	}
	return res
}

// multipartBody builds a multipart form with one file field. contentType ""
// leaves out the part's Content-Type header.
func multipartBody(t *testing.T, field, fileName, contentType string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, fileName))
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	part, err := mw.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func upload(t *testing.T, h http.Handler, fileName, contentType string, content []byte) response {
	t.Helper()
	body, formType := multipartBody(t, "file", fileName, contentType, content)
	return send(t, h, http.MethodPost, "/receipts", formType, body)
}

// uploadFixture uploads fixtures/<folder>/<name>.txt as text/plain and
// returns the receipt id.
func uploadFixture(t *testing.T, h http.Handler, folder, name string) string {
	t.Helper()
	content, err := os.ReadFile(fixturePath(folder, name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	res := upload(t, h, name+".txt", "text/plain", content)
	if res.status != http.StatusCreated {
		t.Fatalf("upload %s: %d %s", name, res.status, res.raw)
	}
	return res.str("receipt_id")
}

func sendJSON(t *testing.T, h http.Handler, method, path, body string) response {
	t.Helper()
	return send(t, h, method, path, "application/json", strings.NewReader(body))
}

func wantError(t *testing.T, what string, res response, status int, code string) {
	t.Helper()
	if res.status != status || res.str("error") != code || res.str("message") == "" {
		t.Errorf("%s: %d %s, want %d with error %s and a message", what, res.status, res.raw, status, code)
	}
}

func TestHealth(t *testing.T) {
	h := newAPI(t, nil)
	res := send(t, h, http.MethodGet, "/health", "", nil)
	if res.status != http.StatusOK || res.str("status") != "ok" {
		t.Errorf("GET /health = %d %s", res.status, res.raw)
	}
}

// gold mirrors one entry of gold.json.
type gold struct {
	Merchant      string          `json:"merchant"`
	Date          string          `json:"date"`
	Currency      string          `json:"currency"`
	GrandTotal    decimal.Decimal `json:"grand_total"`
	ItemizeStatus string          `json:"itemize_status"`
	Taxes         []struct {
		Name   string          `json:"name"`
		Rate   decimal.Decimal `json:"rate"`
		Amount decimal.Decimal `json:"amount"`
	} `json:"taxes"`
	LineItems []struct {
		Description string          `json:"description"`
		Amount      decimal.Decimal `json:"amount"`
	} `json:"line_items"`
}

// sameDecimal reports whether s, a JSON money or rate string, equals want.
func sameDecimal(s any, want decimal.Decimal) bool {
	str, ok := s.(string)
	if !ok {
		return false
	}
	got, err := decimal.NewFromString(str)
	return err == nil && got.Equal(want)
}

// money is how the API writes an amount: a string with exactly two decimals.
var money = regexp.MustCompile(`^-?\d+\.\d{2}$`)

// checkMoney fails the test unless every amount in an expense body is written
// as money: "24.00", never "24", "24.0" or the number 24.
func checkMoney(t *testing.T, name string, body map[string]any) {
	t.Helper()
	amounts := []any{body["grand_total"]}
	taxes, _ := body["taxes"].([]any)
	for _, tax := range taxes {
		tax, _ := tax.(map[string]any)
		amounts = append(amounts, tax["amount"])
		if tax["taxable_amount"] != nil {
			amounts = append(amounts, tax["taxable_amount"])
		}
	}
	items, _ := body["line_items"].([]any)
	for _, item := range items {
		item, _ := item.(map[string]any)
		amounts = append(amounts, item["amount"])
	}
	for _, amount := range amounts {
		if s, ok := amount.(string); !ok || !money.MatchString(s) {
			t.Errorf("%s: amount %#v is not a string with two decimals", name, amount)
		}
	}
}

func TestFixturesEndToEnd(t *testing.T) {
	data, err := os.ReadFile(fixturePath("task-a", "gold.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golds map[string]gold
	if err := json.Unmarshal(data, &golds); err != nil {
		t.Fatal(err)
	}

	h := newAPI(t, nil)
	for name, want := range golds {
		receiptID := uploadFixture(t, h, "task-a", name)

		processed := send(t, h, http.MethodPost, "/receipts/"+receiptID+"/process", "", nil)
		if processed.status != http.StatusOK {
			t.Fatalf("%s: process = %d %s", name, processed.status, processed.raw)
		}
		b := processed.body
		if b["merchant"] != want.Merchant || b["date"] != want.Date || b["currency"] != want.Currency ||
			!sameDecimal(b["grand_total"], want.GrandTotal) || b["itemize_status"] != want.ItemizeStatus {
			t.Errorf("%s: header %s, want gold %+v", name, processed.raw, want)
		}
		checkMoney(t, name, b)

		taxes, _ := b["taxes"].([]any)
		if len(taxes) != len(want.Taxes) {
			t.Errorf("%s: %d taxes, want %d", name, len(taxes), len(want.Taxes))
		}
		for i := 0; i < len(taxes) && i < len(want.Taxes); i++ {
			tax := taxes[i].(map[string]any)
			w := want.Taxes[i]
			if tax["name"] != w.Name || !sameDecimal(tax["rate"], w.Rate) || !sameDecimal(tax["amount"], w.Amount) {
				t.Errorf("%s: tax %d = %v, want %+v", name, i, tax, w)
			}
		}

		items, ok := b["line_items"].([]any)
		if !ok || len(items) != len(want.LineItems) {
			t.Errorf("%s: line_items = %v, want %d items (an array, even when empty)", name, b["line_items"], len(want.LineItems))
		}
		for i := 0; i < len(items) && i < len(want.LineItems); i++ {
			item := items[i].(map[string]any)
			if item["description"] != want.LineItems[i].Description || !sameDecimal(item["amount"], want.LineItems[i].Amount) {
				t.Errorf("%s: item %d = %v, want %+v", name, i, item, want.LineItems[i])
			}
		}

		// GET /transactions/{id} returns exactly what process returned.
		got := send(t, h, http.MethodGet, "/transactions/"+processed.str("id"), "", nil)
		if got.status != http.StatusOK || got.raw != processed.raw {
			t.Errorf("%s: GET transaction = %d %s\nwant %s", name, got.status, got.raw, processed.raw)
		}

		status := send(t, h, http.MethodGet, "/receipts/"+receiptID, "", nil)
		if status.str("status") != receipt.StatusProcessed || status.body["failure_reason"] != nil ||
			status.str("expense_id") != processed.str("id") {
			t.Errorf("%s: GET receipt = %s", name, status.raw)
		}
	}
}

func TestUploadResponse(t *testing.T) {
	h := newAPI(t, nil)
	res := upload(t, h, "receipt-clean.txt", "text/plain; charset=utf-8", []byte("MERCHANT: x"))
	if res.status != http.StatusCreated || res.str("status") != receipt.StatusUploaded || res.str("receipt_id") == "" {
		t.Fatalf("upload = %d %s", res.status, res.raw)
	}
	if loc := res.header.Get("Location"); loc != "/receipts/"+res.str("receipt_id") {
		t.Errorf("Location = %q", loc)
	}
	status := send(t, h, http.MethodGet, "/receipts/"+res.str("receipt_id"), "", nil)
	file, _ := status.body["file"].(map[string]any)
	if file["file_name"] != "receipt-clean.txt" || file["content_type"] != "text/plain" || status.body["expense_id"] != nil {
		t.Errorf("GET receipt after upload = %s; want the bare media type and expense_id null", status.raw)
	}
}

func TestGuardFixtures(t *testing.T) {
	h := newAPI(t, nil)
	for name, code := range map[string]string{
		"unreadable":        receipt.CodeOCRUnreadable,
		"not-a-receipt":     receipt.CodeNotAReceipt,
		"header-incomplete": receipt.CodeHeaderIncomplete,
		"invalid-values":    receipt.CodeInvalidReceiptValues,
	} {
		receiptID := uploadFixture(t, h, "mock-ocr", name)
		res := send(t, h, http.MethodPost, "/receipts/"+receiptID+"/process", "", nil)
		wantError(t, name, res, http.StatusUnprocessableEntity, code)

		status := send(t, h, http.MethodGet, "/receipts/"+receiptID, "", nil)
		if status.str("status") != receipt.StatusFailed || status.str("failure_reason") != code || status.body["expense_id"] != nil {
			t.Errorf("%s: GET receipt = %s", name, status.raw)
		}
	}
}

func TestPatchAndReitemize(t *testing.T) {
	h := newAPI(t, nil)
	receiptID := uploadFixture(t, h, "task-a", "receipt-mismatch")
	processed := send(t, h, http.MethodPost, "/receipts/"+receiptID+"/process", "", nil)
	id := processed.str("id")
	items := processed.body["line_items"].([]any)
	water := items[0].(map[string]any)["id"].(string)
	snacks := items[1].(map[string]any)["id"].(string)
	path := "/transactions/" + id + "/items"

	// Water and Snacks alone don't reach the total: 409 with the numbers.
	res := sendJSON(t, h, http.MethodPatch, path, fmt.Sprintf(
		`[{"id":%q,"description":"Water","amount":"4.00"},{"id":%q,"description":"Snacks","amount":"6.00"}]`, water, snacks))
	wantError(t, "mismatch", res, http.StatusConflict, "ITEMS_DO_NOT_RECONCILE")
	if res.str("expected") != "18.50" || res.str("actual") != "11.90" || res.str("difference") != "6.60" {
		t.Errorf("mismatch numbers = %s", res.raw)
	}

	for what, body := range map[string]string{
		"malformed JSON":  `[{"description":`,
		"not an array":    `{"description":"Water","amount":"4.00"}`,
		"null":            `null`,
		"unknown field":   `[{"description":"Water","amount":"4.00","tax_amount":"0.76"}]`,
		"data after body": `[] []`,
	} {
		wantError(t, what, sendJSON(t, h, http.MethodPatch, path, body), http.StatusBadRequest, "INVALID_BODY")
	}
	wantError(t, "unknown id", sendJSON(t, h, http.MethodPatch, path,
		`[{"id":"no-such-item","description":"Water","amount":"18.50"}]`), http.StatusBadRequest, "UNKNOWN_ITEM")
	wantError(t, "zero amount", sendJSON(t, h, http.MethodPatch, path,
		`[{"description":"Water","amount":0}]`), http.StatusBadRequest, "INVALID_ITEM")

	// Adding the missing 6.60 reconciles. A numeric amount works too.
	res = sendJSON(t, h, http.MethodPatch, path, fmt.Sprintf(
		`[{"id":%q,"description":"Water","amount":"4.00"},{"id":%q,"description":"Snacks","amount":"6.00"},{"description":"Minibar","amount":6.60}]`,
		water, snacks))
	if res.status != http.StatusOK || res.str("itemize_status") != expense.StatusComplete {
		t.Fatalf("PATCH that reconciles = %d %s", res.status, res.raw)
	}

	// Re-itemize goes back to the automatic result.
	res = send(t, h, http.MethodPost, "/transactions/"+id+"/itemize", "", nil)
	if res.status != http.StatusOK || res.str("itemize_status") != expense.StatusNeedsReview || res.str("id") != id {
		t.Errorf("re-itemize = %d %s", res.status, res.raw)
	}
}

func TestReprocess(t *testing.T) {
	h := newAPI(t, nil)
	receiptID := uploadFixture(t, h, "task-a", "receipt-clean")
	first := send(t, h, http.MethodPost, "/receipts/"+receiptID+"/process", "", nil).str("id")
	second := send(t, h, http.MethodPost, "/receipts/"+receiptID+"/process", "", nil).str("id")

	if first == second {
		t.Fatal("reprocessing kept the transaction id")
	}
	wantError(t, "old transaction", send(t, h, http.MethodGet, "/transactions/"+first, "", nil), http.StatusNotFound, "NOT_FOUND")
	if got := send(t, h, http.MethodGet, "/receipts/"+receiptID, "", nil).str("expense_id"); got != second {
		t.Errorf("receipt expense_id = %s, want the new %s", got, second)
	}
}

func TestNotFound(t *testing.T) {
	h := newAPI(t, nil)
	for _, req := range []struct{ method, path string }{
		{http.MethodPost, "/receipts/no-such-id/process"},
		{http.MethodGet, "/receipts/no-such-id"},
		{http.MethodGet, "/transactions/no-such-id"},
		{http.MethodPost, "/transactions/no-such-id/itemize"},
		{http.MethodGet, "/no/such/path"},
	} {
		wantError(t, req.method+" "+req.path, send(t, h, req.method, req.path, "", nil), http.StatusNotFound, "NOT_FOUND")
	}
	wantError(t, "PATCH unknown transaction",
		sendJSON(t, h, http.MethodPatch, "/transactions/no-such-id/items", `[]`), http.StatusNotFound, "NOT_FOUND")
}

func TestMethodNotAllowed(t *testing.T) {
	h := newAPI(t, nil)
	res := send(t, h, http.MethodDelete, "/receipts/some-id", "", nil)
	wantError(t, "DELETE /receipts/{id}", res, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
	if allow := res.header.Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want \"GET, HEAD\"", allow)
	}
	wantError(t, "GET /receipts", send(t, h, http.MethodGet, "/receipts", "", nil), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
}

func TestUploadErrors(t *testing.T) {
	h := newAPI(t, nil)

	body, formType := multipartBody(t, "document", "receipt.txt", "text/plain", []byte("hello"))
	wantError(t, "no file field", send(t, h, http.MethodPost, "/receipts", formType, body), http.StatusBadRequest, "FILE_MISSING")
	wantError(t, "empty file", upload(t, h, "receipt.txt", "text/plain", nil), http.StatusBadRequest, "FILE_EMPTY")
	wantError(t, "not multipart", sendJSON(t, h, http.MethodPost, "/receipts", `{"file":"x"}`), http.StatusBadRequest, "INVALID_UPLOAD")

	wantError(t, "zip", upload(t, h, "receipt.zip", "application/zip", []byte("PK")), http.StatusUnsupportedMediaType, "UNSUPPORTED_FILE_TYPE")
	wantError(t, "no content type", upload(t, h, "receipt", "", []byte("data")), http.StatusUnsupportedMediaType, "UNSUPPORTED_FILE_TYPE")

	// 10 MB + 1 byte fits in the body limit, so the size check catches it.
	justOver := bytes.Repeat([]byte("a"), maxUploadBytes+1)
	wantError(t, "file just over 10 MB", upload(t, h, "big.txt", "text/plain", justOver), http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE")
	// A body over the 11 MB cap is cut off while reading.
	huge := bytes.Repeat([]byte("a"), maxUploadBody+1)
	wantError(t, "body over the cap", upload(t, h, "huge.txt", "text/plain", huge), http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE")
}

func TestLiveOCRNotConfigured(t *testing.T) {
	h := newAPI(t, ocr.LiveProvider{})
	receiptID := uploadFixture(t, h, "task-a", "receipt-clean")
	res := send(t, h, http.MethodPost, "/receipts/"+receiptID+"/process", "", nil)
	wantError(t, "live OCR", res, http.StatusNotImplemented, "LIVE_OCR_NOT_CONFIGURED")
}

func TestMockFallback(t *testing.T) {
	h := newAPI(t, nil)
	res := upload(t, h, "photo.png", "image/png", []byte("fake png bytes"))
	processed := send(t, h, http.MethodPost, "/receipts/"+res.str("receipt_id")+"/process", "", nil)
	if processed.status != http.StatusOK || processed.str("merchant") != "Cafe Mitte" ||
		processed.str("itemize_status") != expense.StatusComplete {
		t.Errorf("fallback = %d %s; want the gold receipt", processed.status, processed.raw)
	}
}

func TestInternalErrorsDontLeak(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), errors.New("sqlite: disk I/O error at /secret/path"))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") ||
		!strings.Contains(rec.Body.String(), "INTERNAL_ERROR") {
		t.Errorf("unexpected error = %d %s; want a generic 500", rec.Code, rec.Body.String())
	}
}

func TestPanicBecomesJSON500(t *testing.T) {
	h := recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }))
	res := send(t, h, http.MethodGet, "/", "", nil)
	wantError(t, "panic", res, http.StatusInternalServerError, "INTERNAL_ERROR")
}
