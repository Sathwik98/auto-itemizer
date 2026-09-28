package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"auto-itemizer/internal/db"
	expensecore "auto-itemizer/internal/expense/core"
	expenseservice "auto-itemizer/internal/expense/service"
	fileuploadcore "auto-itemizer/internal/fileupload/core"
	fileuploadservice "auto-itemizer/internal/fileupload/service"
	"auto-itemizer/internal/httpapi/respond"
	"auto-itemizer/internal/metrics"
	"auto-itemizer/internal/ocr"
	receiptcore "auto-itemizer/internal/receipt/core"
	"auto-itemizer/internal/receipt/core/parser"
	receiptserver "auto-itemizer/internal/receipt/server"
	receiptservice "auto-itemizer/internal/receipt/service"

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
	files, err := fileuploadservice.New(t.TempDir())
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
	expenses := expenseservice.New(d)
	names, err := expenses.TaxNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	receipts := receiptservice.New(d, files, ocr.New(provider, time.Second), expenses, parser.New(names))
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
		if status.str("status") != receiptcore.StatusProcessed || status.body["failure_reason"] != nil ||
			status.str("expense_id") != processed.str("id") {
			t.Errorf("%s: GET receipt = %s", name, status.raw)
		}
	}
}

func TestUploadResponse(t *testing.T) {
	h := newAPI(t, nil)
	res := upload(t, h, "receipt-clean.txt", "text/plain; charset=utf-8", []byte("MERCHANT: x"))
	if res.status != http.StatusCreated || res.str("status") != receiptcore.StatusUploaded || res.str("receipt_id") == "" {
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
		"unreadable":        receiptcore.CodeOCRUnreadable,
		"not-a-receipt":     receiptcore.CodeNotAReceipt,
		"header-incomplete": receiptcore.CodeHeaderIncomplete,
		"invalid-values":    receiptcore.CodeInvalidReceiptValues,
	} {
		receiptID := uploadFixture(t, h, "mock-ocr", name)
		res := send(t, h, http.MethodPost, "/receipts/"+receiptID+"/process", "", nil)
		wantError(t, name, res, http.StatusUnprocessableEntity, code)

		status := send(t, h, http.MethodGet, "/receipts/"+receiptID, "", nil)
		if status.str("status") != receiptcore.StatusFailed || status.str("failure_reason") != code || status.body["expense_id"] != nil {
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
	wantError(t, "blank description", sendJSON(t, h, http.MethodPatch, path,
		`[{"description":"   ","amount":"16.60"}]`), http.StatusBadRequest, "INVALID_ITEM")

	// Adding the missing 6.60 reconciles. A numeric amount works too.
	res = sendJSON(t, h, http.MethodPatch, path, fmt.Sprintf(
		`[{"id":%q,"description":"Water","amount":"4.00"},{"id":%q,"description":"Snacks","amount":"6.00"},{"description":"Minibar","amount":6.60}]`,
		water, snacks))
	if res.status != http.StatusOK || res.str("itemize_status") != expensecore.StatusComplete {
		t.Fatalf("PATCH that reconciles = %d %s", res.status, res.raw)
	}

	// Re-itemize goes back to the automatic result.
	res = send(t, h, http.MethodPost, "/transactions/"+id+"/itemize", "", nil)
	if res.status != http.StatusOK || res.str("itemize_status") != expensecore.StatusNeedsReview || res.str("id") != id {
		t.Errorf("re-itemize = %d %s", res.status, res.raw)
	}
}

// lineItems returns the ids and descriptions of an expense response's items.
func lineItems(res response) (ids, names []string) {
	items, _ := res.body["line_items"].([]any)
	for _, item := range items {
		item, _ := item.(map[string]any)
		id, _ := item["id"].(string)
		name, _ := item["description"].(string)
		ids = append(ids, id)
		names = append(names, name)
	}
	return ids, names
}

// TestPatchEditMergeSplit runs the brief's three kinds of override (split,
// merge and edit) through PATCH /transactions/{id}/items on receipt-clean.
func TestPatchEditMergeSplit(t *testing.T) {
	h := newAPI(t, nil)
	processed := send(t, h, http.MethodPost, "/receipts/"+uploadFixture(t, h, "task-a", "receipt-clean")+"/process", "", nil)
	transaction := "/transactions/" + processed.str("id")

	// patch sends a list that reconciles and checks the items that come back,
	// in order: unchanged items first, then changed and new ones.
	patch := func(what, body string, want ...string) []string {
		t.Helper()
		res := sendJSON(t, h, http.MethodPatch, transaction+"/items", body)
		if res.status != http.StatusOK || res.str("itemize_status") != expensecore.StatusComplete {
			t.Fatalf("%s = %d %s", what, res.status, res.raw)
		}
		checkMoney(t, what, res.body)
		ids, names := lineItems(res)
		if !slices.Equal(names, want) {
			t.Errorf("%s: items %v, want %v", what, names, want)
		}
		return ids
	}

	// Split: the Sandwich (8.90) becomes Bread 5.00 and Cheese 3.90.
	ids, _ := lineItems(processed) // Espresso, Sandwich, Mineral water
	ids = patch("split", fmt.Sprintf(
		`[{"id":%q,"description":"Espresso","amount":"3.50"},{"description":"Bread","amount":"5.00"},{"description":"Cheese","amount":"3.90"},{"id":%q,"description":"Mineral water","amount":"2.60"}]`,
		ids[0], ids[2]), "Espresso", "Mineral water", "Bread", "Cheese")

	// Merge: Bread and Cheese become one line again.
	ids = patch("merge", fmt.Sprintf(
		`[{"id":%q,"description":"Espresso","amount":"3.50"},{"id":%q,"description":"Mineral water","amount":"2.60"},{"description":"Sandwich","amount":"8.90"}]`,
		ids[0], ids[1]), "Espresso", "Mineral water", "Sandwich")

	// Edit: move 0.50 from Espresso to Mineral water. Sandwich is unchanged, so it comes first.
	ids = patch("edit", fmt.Sprintf(
		`[{"id":%q,"description":"Espresso","amount":"3.00"},{"id":%q,"description":"Mineral water","amount":"3.10"},{"id":%q,"description":"Sandwich","amount":"8.90"}]`,
		ids[0], ids[1], ids[2]), "Sandwich", "Espresso", "Mineral water")

	// A split whose parts are 0.90 short is refused, and nothing changes.
	before := send(t, h, http.MethodGet, transaction, "", nil)
	res := sendJSON(t, h, http.MethodPatch, transaction+"/items", fmt.Sprintf(
		`[{"id":%q,"description":"Espresso","amount":"3.00"},{"id":%q,"description":"Mineral water","amount":"3.10"},{"description":"Bread","amount":"5.00"},{"description":"Cheese","amount":"3.00"}]`,
		ids[1], ids[2]))
	wantError(t, "uneven split", res, http.StatusConflict, "ITEMS_DO_NOT_RECONCILE")
	if res.str("difference") != "0.90" {
		t.Errorf("uneven split difference = %s, want 0.90", res.raw)
	}
	if after := send(t, h, http.MethodGet, transaction, "", nil); after.raw != before.raw {
		t.Errorf("a refused PATCH changed the transaction:\n%s\nwant\n%s", after.raw, before.raw)
	}
}

// TestUploadImagesAndPDF: POST /receipts takes an image or a PDF (the brief),
// and GET /receipts/{id} reports the type that was sent.
func TestUploadImagesAndPDF(t *testing.T) {
	h := newAPI(t, nil)
	for name, contentType := range map[string]string{
		"receipt.png": "image/png",
		"receipt.jpg": "image/jpeg",
		"receipt.pdf": "application/pdf",
	} {
		res := upload(t, h, name, contentType, []byte("fake bytes"))
		if res.status != http.StatusCreated {
			t.Errorf("%s: upload = %d %s, want 201", name, res.status, res.raw)
			continue
		}
		got := send(t, h, http.MethodGet, "/receipts/"+res.str("receipt_id"), "", nil)
		if file, _ := got.body["file"].(map[string]any); file["content_type"] != contentType {
			t.Errorf("%s: GET receipt = %s, want content_type %s", name, got.raw, contentType)
		}
	}
}

// TestExtraFixtures runs our extra receipts in fixtures/mock-ocr, beyond the
// brief's three: two taxes on one subtotal (Canada), CGST and SGST (India),
// and a discount line.
func TestExtraFixtures(t *testing.T) {
	h := newAPI(t, nil)
	cases := []struct {
		fixture, merchant, currency, total string
		taxes                              []string // "name rate taxable amount"; taxable is null when the receipt doesn't show it
		items                              []string // "description amount"
	}{
		{"receipt-gst-qst", "Poutine Palace", "CAD", "14.38",
			[]string{"GST 0.05 null 0.63", "QST 0.09975 null 1.25"},
			[]string{"Poutine 10.00", "Soft drink 2.50"}},
		{"receipt-cgst-sgst", "Chennai Tiffin House", "INR", "212.40",
			[]string{"CGST 0.09 null 16.20", "SGST 0.09 null 16.20"},
			[]string{"Masala dosa 120.00", "Filter coffee 60.00"}},
		{"receipt-discount", "Schreibwaren Kiez", "EUR", "14.99",
			[]string{"VAT 0.19 12.60 2.39"},
			[]string{"Notebook 8.00", "Pen set 6.00", "Discount 10% -1.40"}},
	}
	for _, tc := range cases {
		res := send(t, h, http.MethodPost, "/receipts/"+uploadFixture(t, h, "mock-ocr", tc.fixture)+"/process", "", nil)
		if res.status != http.StatusOK {
			t.Errorf("%s: process = %d %s", tc.fixture, res.status, res.raw)
			continue
		}
		checkMoney(t, tc.fixture, res.body)

		var taxes, items []string
		for _, tax := range res.body["taxes"].([]any) {
			tax := tax.(map[string]any)
			taxable := "null"
			if s, ok := tax["taxable_amount"].(string); ok {
				taxable = s
			}
			taxes = append(taxes, fmt.Sprintf("%s %s %s %s", tax["name"], tax["rate"], taxable, tax["amount"]))
		}
		for _, item := range res.body["line_items"].([]any) {
			item := item.(map[string]any)
			items = append(items, fmt.Sprintf("%s %s", item["description"], item["amount"]))
		}
		if res.str("merchant") != tc.merchant || res.str("currency") != tc.currency || res.str("grand_total") != tc.total ||
			res.str("itemize_status") != expensecore.StatusComplete || !slices.Equal(taxes, tc.taxes) || !slices.Equal(items, tc.items) {
			t.Errorf("%s = %s\nwant %s %s %s, taxes %v, items %v, COMPLETE", tc.fixture, res.raw, tc.merchant, tc.currency, tc.total, tc.taxes, tc.items)
		}
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
	justOver := bytes.Repeat([]byte("a"), receiptserver.MaxUploadBytes+1)
	wantError(t, "file just over 10 MB", upload(t, h, "big.txt", "text/plain", justOver), http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE")
	// A body over the 11 MB cap is cut off while reading.
	huge := bytes.Repeat([]byte("a"), receiptserver.MaxUploadBody+1)
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
		processed.str("itemize_status") != expensecore.StatusComplete {
		t.Errorf("fallback = %d %s; want the gold receipt", processed.status, processed.raw)
	}
}

// captureLogs sends slog's output to a buffer until the test ends. The
// default logger is global, so tests that use this must not run in parallel.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// wantLog fails the test unless the captured logs contain every part.
func wantLog(t *testing.T, logs *bytes.Buffer, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(logs.String(), part) {
			t.Errorf("the logs lack %q; they are:\n%s", part, logs.String())
		}
	}
}

// metricValue returns one series' value from /metrics, or 0 when the series
// isn't there yet. Counters are process-wide, so tests compare before and
// after.
func metricValue(t *testing.T, series string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
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

// wantIncrease fails the test unless series went up by want since before.
func wantIncrease(t *testing.T, series string, before, want float64) {
	t.Helper()
	if got := metricValue(t, series) - before; got != want {
		t.Errorf("%s went up by %v, want %v", series, got, want)
	}
}

func TestInternalErrorsDontLeak(t *testing.T) {
	before := metricValue(t, "internal_errors_total")
	rec := httptest.NewRecorder()
	respond.WriteError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), errors.New("sqlite: disk I/O error at /secret/path"))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") ||
		!strings.Contains(rec.Body.String(), "INTERNAL_ERROR") {
		t.Errorf("unexpected error = %d %s; want a generic 500", rec.Code, rec.Body.String())
	}
	wantIncrease(t, "internal_errors_total", before, 1)
}

// A write conflict is answered with 409, counted and logged.
func TestWriteConflictIsCountedAndLogged(t *testing.T) {
	logs := captureLogs(t)
	before := metricValue(t, "write_conflicts_total")
	rec := httptest.NewRecorder()
	respond.WriteError(rec, httptest.NewRequest(http.MethodPatch, "/transactions/t1/items", nil), fmt.Errorf("save: %w", db.ErrConflict))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"CONFLICT"`) {
		t.Errorf("conflict = %d %s, want 409 CONFLICT", rec.Code, rec.Body.String())
	}
	wantIncrease(t, "write_conflicts_total", before, 1)
	wantLog(t, logs, `level=WARN msg="write conflict" method=PATCH path=/transactions/t1/items`)
}

// A panic is answered with a JSON 500, counted, and its request logged at ERROR.
func TestPanicBecomesJSON500(t *testing.T) {
	logs := captureLogs(t)
	before := metricValue(t, "panics_total")
	h := logRequests(recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })))
	res := send(t, h, http.MethodGet, "/", "", nil)
	wantError(t, "panic", res, http.StatusInternalServerError, "INTERNAL_ERROR")
	wantIncrease(t, "panics_total", before, 1)
	wantLog(t, logs, `level=ERROR msg="panic in handler"`,
		`level=ERROR msg=request method=GET path=/ route=unmatched status=500`)
}

// Every request gets one log line with the level set by its status, and is
// counted by route and status. GET /metrics is counted but not logged.
func TestRequestLogAndMetrics(t *testing.T) {
	h := newAPI(t, nil)
	logs := captureLogs(t)
	health := `http_requests_total{route="GET /health",status="200"}`
	unknown := `http_requests_total{route="unmatched",status="404"}`
	healthBefore, unknownBefore := metricValue(t, health), metricValue(t, unknown)

	send(t, h, http.MethodGet, "/health", "", nil)
	send(t, h, http.MethodGet, "/no/such/path", "", nil)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))

	wantIncrease(t, health, healthBefore, 1)
	wantIncrease(t, unknown, unknownBefore, 1)
	wantLog(t, logs,
		`level=INFO msg=request method=GET path=/health route="GET /health" status=200 duration_ms=`,
		`level=WARN msg=request method=GET path=/no/such/path route=unmatched status=404`)
	if strings.Contains(logs.String(), "path=/metrics") {
		t.Errorf("GET /metrics was logged:\n%s", logs.String())
	}
}

// hangUpProvider is an OCR provider for a client that disconnects during OCR:
// it cancels the request's context and returns once that context has ended.
type hangUpProvider struct{ cancel context.CancelFunc }

func (p hangUpProvider) Extract(ctx context.Context, f fileuploadcore.FileUpload) (string, error) {
	p.cancel()
	<-ctx.Done()
	return "", ctx.Err()
}

// A client that leaves before its answer is written is logged and counted
// as 499, not 200, and the receipt is left as it was.
func TestClientGoneIsLoggedAs499(t *testing.T) {
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	h := newAPI(t, hangUpProvider{cancel: cancel})
	receiptID := uploadFixture(t, h, "task-a", "receipt-clean")
	logs := captureLogs(t)
	series := `http_requests_total{route="POST /receipts/{id}/process",status="499"}`
	before := metricValue(t, series)

	req := httptest.NewRequest(http.MethodPost, "/receipts/"+receiptID+"/process", nil).WithContext(requestCtx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	wantIncrease(t, series, before, 1)
	wantLog(t, logs, `level=WARN msg=request method=POST path=/receipts/`+receiptID+`/process route="POST /receipts/{id}/process" status=499`)
	if status := send(t, h, http.MethodGet, "/receipts/"+receiptID, "", nil); status.str("status") != receiptcore.StatusUploaded {
		t.Errorf("after the client left: %s, want the receipt still UPLOADED", status.raw)
	}
}

// GET /metrics answers in the Prometheus text format and lists every metric.
func TestMetricsEndpoint(t *testing.T) {
	h := newAPI(t, nil)
	send(t, h, http.MethodGet, "/health", "", nil) // so the request metrics have a series
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("GET /metrics = %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, name := range []string{"http_requests_total", "http_request_duration_seconds", "receipt_outcomes_total",
		"mock_ocr_fallbacks_total", "patches_refused_total", "write_conflicts_total", "internal_errors_total", "panics_total"} {
		if !strings.Contains(rec.Body.String(), "# TYPE "+name+" ") {
			t.Errorf("/metrics lacks %s", name)
		}
	}
}

// Each processed receipt is logged and counted by its outcome. An unknown
// file name also counts a mock fallback.
func TestReceiptOutcomesAreLoggedAndCounted(t *testing.T) {
	h := newAPI(t, nil)
	logs := captureLogs(t)
	outcome := func(name string) string { return `receipt_outcomes_total{outcome="` + name + `"}` }
	complete, review, letter := metricValue(t, outcome("COMPLETE")), metricValue(t, outcome("NEEDS_REVIEW")), metricValue(t, outcome("NOT_A_RECEIPT"))
	fallbacks := metricValue(t, "mock_ocr_fallbacks_total")
	process := func(receiptID string) response {
		return send(t, h, http.MethodPost, "/receipts/"+receiptID+"/process", "", nil)
	}

	clean := uploadFixture(t, h, "task-a", "receipt-clean")
	cleanTransaction := process(clean).str("id")
	process(uploadFixture(t, h, "task-a", "receipt-mismatch"))
	notReceipt := uploadFixture(t, h, "mock-ocr", "not-a-receipt")
	process(notReceipt)
	process(upload(t, h, "photo.png", "image/png", []byte("fake png bytes")).str("receipt_id")) // falls back to receipt-clean

	wantIncrease(t, outcome("COMPLETE"), complete, 2) // receipt-clean and the fallback
	wantIncrease(t, outcome("NEEDS_REVIEW"), review, 1)
	wantIncrease(t, outcome("NOT_A_RECEIPT"), letter, 1)
	wantIncrease(t, "mock_ocr_fallbacks_total", fallbacks, 1)
	wantLog(t, logs,
		`level=INFO msg="receipt processed" receipt_id=`+clean+` transaction_id=`+cleanTransaction+` itemize_status=COMPLETE`,
		`level=WARN msg="receipt failed a guard" receipt_id=`+notReceipt+` code=NOT_A_RECEIPT reason=`,
		`msg="mock OCR: no fixture matches the uploaded name, using the fallback" file_name=photo.png`)
}

// A refused PATCH is counted and logged with its numbers. An accepted PATCH
// and a re-itemize are logged too.
func TestPatchEventsAreLoggedAndCounted(t *testing.T) {
	h := newAPI(t, nil)
	logs := captureLogs(t)
	refused := metricValue(t, "patches_refused_total")
	processed := send(t, h, http.MethodPost, "/receipts/"+uploadFixture(t, h, "task-a", "receipt-mismatch")+"/process", "", nil)
	id := processed.str("id")
	ids, _ := lineItems(processed)
	path := "/transactions/" + id + "/items"

	sendJSON(t, h, http.MethodPatch, path, fmt.Sprintf(
		`[{"id":%q,"description":"Water","amount":"4.00"},{"id":%q,"description":"Snacks","amount":"6.00"}]`, ids[0], ids[1]))
	sendJSON(t, h, http.MethodPatch, path, fmt.Sprintf(
		`[{"id":%q,"description":"Water","amount":"4.00"},{"id":%q,"description":"Snacks","amount":"6.00"},{"description":"Minibar","amount":"6.60"}]`, ids[0], ids[1]))
	send(t, h, http.MethodPost, "/transactions/"+id+"/itemize", "", nil)

	wantIncrease(t, "patches_refused_total", refused, 1)
	wantLog(t, logs,
		`level=WARN msg="PATCH refused: the items don't add up" transaction_id=`+id+` expected=18.50 actual=11.90 difference=6.60`,
		`level=INFO msg="items replaced by a PATCH" transaction_id=`+id+` items=3`,
		`level=INFO msg="transaction re-itemized" transaction_id=`+id+` itemize_status=NEEDS_REVIEW`)
}
