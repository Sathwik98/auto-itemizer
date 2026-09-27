# Implementation plan — Receipt upload, taxes, auto-itemize

This document covers **how the application is built**. [ARCHITECTURE.md](ARCHITECTURE.md) describes how it behaves: its components, data model, endpoint flows and rules. Section numbers like §3.2 refer to ARCHITECTURE.md.

---

## 1. Prerequisites

- Go 1.25 or later, because the SQLite driver (`modernc.org/sqlite` v1.59) needs it. Developed with go1.27.1.
- No C toolchain is needed, because the SQLite driver is pure Go.

---

## 2. Stack and project layout

**Stack:** Go 1.25 or later, using only the standard library `net/http`, whose router handles methods and path parameters (`POST /receipts/{id}/process`). There are three dependencies:

| Dependency | Why |
|---|---|
| `modernc.org/sqlite` | SQLite driver written in pure Go. It needs no CGO or C compiler, so one `go run` starts the service. |
| `github.com/shopspring/decimal` | Exact decimal arithmetic for money and rates, never `float64` |
| `github.com/google/uuid` | Ids and stored file names |

```
auto-itemizer/
├── cmd/server/main.go        # reads config, wires components, starts HTTP, shuts down cleanly on SIGTERM
├── internal/
│   ├── config/               # env: PORT, DB_PATH, STORAGE_DIR, FIXTURES_DIR, MOCK_OCR, MOCK_OCR_FALLBACK
│   ├── httpapi/              # ReceiptController, ExpenseController: routes, request checks, JSON error body (§3.0)
│   ├── receipt/              # ReceiptService + one repository file per table (receipts, receipt_ocr); guards.go (pure)
│   │   └── parser/           # Parser built with the tax names; Parse(text) → ParsedReceipt (pure); its own package so expense can import it
│   ├── expense/              # ExpenseService + one repository file per table (expense, expense_tax, expense_line_item, tax_master); itemize.go: itemize and reconcile (pure)
│   ├── ocr/                  # OcrService (generic): Provider interface, MockProvider, LiveProvider ("not configured")
│   ├── fileupload/           # FileUploadService + repository for file_upload; writes files to STORAGE_DIR
│   └── db/                   # SQLite connection, embedded schema (.sql), transaction helper passed between services
├── fixtures/
│   ├── task-a/               # given fixtures and gold.json, unchanged
│   └── mock-ocr/             # our guard fixtures (§4 below)
├── storage/                  # uploaded files (gitignored)
├── README.md                 # run with one command, curl for every endpoint
├── ARCHITECTURE.md           # how the application works
└── IMPLEMENTATION_PLAN.md    # this file
```

---

## 3. Storage details

- **Money and rates in SQLite:** these are `NUMERIC(12,2)` / `NUMERIC(6,4)` in the data model. In SQLite they are declared as `TEXT` and hold decimal strings such as `"17.85"`, because SQLite's `NUMERIC` affinity would quietly convert `17.85` into a floating-point number. In Go they are always `decimal.Decimal`.
- **Decimal text format:** money is always written with `StringFixed(2)`, e.g. `"15.00"`, both in SQLite and in JSON. Rates are written to SQLite and returned in JSON with `String()`, exactly as parsed (`"0.19"`, or `"0.09975"` for 9.975%), so a printed rate is never rounded. Never use `String()` or the library's default JSON encoding for money: both drop trailing zeros, so `15.00` would come out as `"15"`.
- **Schema:** `internal/db/schema.sql`, embedded and applied at every startup, with `CREATE TABLE IF NOT EXISTS` and the partial unique indexes from §2. It also has CHECK constraints on `receipts.status`, `expense.itemization_status` and `is_deleted`, and seeds the tax names into `tax_master` (listed under *Tax names for the parser* below). There are no migrations: `CREATE TABLE IF NOT EXISTS` never changes a table that already exists, so after editing `schema.sql`, delete the database file.
- **Other column types:** ids are UUID strings. Timestamps are UTC text in a fixed-width format (`2006-01-02T15:04:05.000000Z`), so they also sort correctly as text. Booleans are `0`/`1`, and `date` is `YYYY-MM-DD`.
- **`updated_at` always changes on a write.** `db.NextUpdatedAt(prev)` returns the current time, or 1µs after `prev` if the clock hasn't moved past it. This keeps the conflict check in ARCHITECTURE.md §3.0 reliable even for two writes in the same microsecond.
- **Connection settings:** `foreign_keys` on; `journal_mode=WAL`, so reads don't wait for a write; `busy_timeout=5000`, so a second writer waits instead of failing; and `_txlock=immediate`, so every transaction takes the write lock at `BEGIN` and concurrent writes wait their turn. Without that last setting, two transactions that read and then write fail with `database is locked`; the db tests check this.
- **Line item order:** items are returned in insertion order (`ORDER BY rowid`), with no position column. `expense_line_item` must therefore stay an ordinary rowid table.
- **Transactions across services:** `db.InTx` runs a function in one `*sql.Tx`. Repository methods take a `db.Querier`, which both `*sql.DB` and `*sql.Tx` implement. `ReceiptService` opens the transaction and passes it to the `ExpenseService` and `FileUploadService` methods it calls, so a save that spans their tables is one transaction (ARCHITECTURE.md §1.1). A conditional write that matches no row returns `db.ErrConflict` (→ 409). Reads that must agree with each other, like the header, taxes and items of one expense, run in `db.InReadTx`: a read-only transaction that sees one snapshot and doesn't take the write lock, because the driver ignores `_txlock=immediate` for read-only transactions.
- **ReceiptService ↔ ExpenseService:** they call each other (§1). Go packages can't import each other, so `expense` defines a small interface, `ParsedReceiptSource { GetParsedReceipt(ctx, receiptID) (parser.ParsedReceipt, error) }`. `ReceiptService` implements it, and `main.go` wires the two together. The parser and the `ParsedReceipt` type live in their own package, `internal/receipt/parser`. They can't be in `receipt` itself: `receipt` imports `expense`, so `expense` naming a type from `receipt` would be an import cycle. Imports go one way: `receipt` → `expense`, both of them → `parser`, and `parser` imports no other package of ours.
- **Tax names for the parser:** the parser is built with `parser.New(taxNames)` and never reads the database itself. In the service the names are the rows of `tax_master`: `main.go` asks ExpenseService for them once at startup and passes the parser to ReceiptService. `schema.sql` seeds VAT, GST, HST, PST, QST, MWST, UST, TVA, IVA, TAX, SALES TAX, CGST, SGST, IGST, UTGST and CESS; the seed runs at every startup with `ON CONFLICT DO NOTHING`, so an existing database picks up new names. Names added while the service runs are recognised after the next restart.
- **Fixtures:** the brief expects them at `fixtures/task-a/` in the repo root. Copy them there from `task-a/fixtures/task-a/` unchanged, and leave the `task-a/` folder as it is.

---

## 4. Test fixtures

We add these guard fixtures in `fixtures/mock-ocr/` so guards 2–5 in §1.3 can be run with curl. Uploading a file with the same stem, e.g. `not-a-receipt.png`, triggers it through the mock OCR (§1.2). Guard 1 (`OCR_FAILED`) can't be triggered this way, because the mock provider never returns an error; it is tested in Go with a fake provider (§5).

The folder must exist before the server starts, because the mock provider fails at startup when a fixture folder is missing. The fixtures were created together with the parser and guards (§6), whose tests check each one gives its code.

| File | Contents | Expected |
|---|---|---|
| `unreadable.txt` | Empty or only noise characters | 422 `OCR_UNREADABLE` |
| `not-a-receipt.txt` | Readable text with no total or amounts, e.g. a letter | 422 `NOT_A_RECEIPT` |
| `header-incomplete.txt` | Has a `TOTAL` line but no merchant or date | 422 `HEADER_INCOMPLETE` |
| `invalid-values.txt` | e.g. total `0.00`, or tax larger than the total | 422 `INVALID_RECEIPT_VALUES` |

---

## 5. Tests

- **Unit tests** for the pure functions: the parser in `receipt/parser`, the guards in `receipt`, itemize and reconcile in `expense`.
- **Golden tests:** the parser's test compares each fixture's header, taxes and candidate lines with `gold.json`. ExpenseService's test runs each fixture through parse → itemize and compares the items and `itemize_status`. The guards are tested on the same fixtures in `receipt`.
- **API tests** use `httptest` against a temporary SQLite database. They cover upload → process → get for the three fixtures, re-itemize, `PATCH` returning 409 and 200, reprocessing, every guard fixture, the mock fallback (an unknown file name gets the gold text), upload errors (400, 413, 415), 404 for unknown and soft-deleted ids, 501 with `MOCK_OCR=false`, `GET /receipts/{id}` after a failure and after reprocessing, and `GET /health`. They also cover the `PATCH` body errors (`INVALID_BODY`, `INVALID_ITEM`, `UNKNOWN_ITEM`), the JSON 404 and 405 for an unknown path or a wrong method, a panic or unexpected error becoming a generic 500, and every amount in a response having exactly two decimals.
- **Service tests** cover what the API can't trigger: `OCR_FAILED` with a fake OCR provider that returns an error, and `409 CONFLICT` with a stale `updated_at`.

---

## 6. Component plans

Components are built bottom-up, in import order: a package is built after the packages it imports. `receipt` imports `expense`, so ExpenseService comes before ReceiptService (§3). Each component's plan is written in plan mode and reviewed with gstack's `/plan-eng-review` before it is coded.

| Component | Package | Status |
|---|---|---|
| Project setup (module, fixtures copy, git) | — | done, except the first git commit (waiting on the git name and email) |
| Database (connection, schema, transactions) | `internal/db` | done |
| FileUploadService | `internal/fileupload` | done |
| OcrService (generic, mock and live providers) | `internal/ocr` | done |
| Receipt parser and guards, and the guard fixtures in `fixtures/mock-ocr/` (§4) | `internal/receipt/parser`, `internal/receipt` (`guards.go`) | done |
| ExpenseService (create, get, re-itemize, patch, itemize, reconcile), one repository file per table | `internal/expense` | done |
| ReceiptService (upload, process, status, `getParsedReceipt`), one repository file per table | `internal/receipt` | done |
| Controllers (ReceiptController, ExpenseController) | `internal/httpapi` | done |
| Config and startup (wiring, graceful shutdown) | `cmd/server`, `internal/config` | next |
| README with curls | — | to do |

**Notes for later components** (from the review on 2026-09-27):
- **ReceiptService (done):**
  - `ocr.ErrNotConfigured` → `ErrLiveOCRNotConfigured` (501), with nothing written.
  - A client that disconnects during OCR gets its context error, and nothing is written.
  - Any other OCR error or a timeout is `OCR_FAILED`, which is saved.
  - Every `receipts` write is conditional on `updated_at`.
- **Controllers (done), error mapping** in `writeError` (`internal/httpapi/respond.go`). Every code a client can see is listed in ARCHITECTURE.md §3.0.

  | Service error | Response |
  |---|---|
  | `receipt.ErrNotFound`, `expense.ErrNotFound` | 404 `NOT_FOUND` |
  | `receipt.ErrLiveOCRNotConfigured` | 501 `LIVE_OCR_NOT_CONFIGURED` |
  | `*receipt.GuardError` | 422 with its `Code` and `Message` |
  | `db.ErrConflict` | 409 `CONFLICT` |
  | `*expense.MismatchError` | 409 `ITEMS_DO_NOT_RECONCILE` with `expected`, `actual` and `difference` |
  | `expense.ErrUnknownItem`, `expense.ErrInvalidItem` | 400 `UNKNOWN_ITEM`, 400 `INVALID_ITEM` |
  | `context.Canceled` | nothing written (the client is gone) |
  | anything else | 500 `INTERNAL_ERROR`, logged with `slog`; the response says only `internal error` |
- **Controllers (done):** read the whole upload with `http.MaxBytesReader` and `r.FormFile` before calling `upload`. The upload transaction holds the SQLite write lock while `Save` copies the file, so streaming from a slow client would block every other write. Pass `Save` the bare media type from `mime.ParseMediaType` (`text/plain`, not `text/plain; charset=utf-8`), because the stored file's extension is looked up by exact type.
- **ExpenseService (done):**
  - `TaxNames` returns the `tax_master` names.
  - Rates are stored exactly.
  - `FAILED` is reserved, because the stub parser never fails.
- **Config and startup:** load the tax names once at startup with `expenses.TaxNames`, build the parser with `parser.New`, and pass it to ReceiptService. Build ExpenseService first (`expense.New`), then ReceiptService with it, then call `expenses.SetParsedReceiptSource(receipts)`: the two services need each other. To propose in that step's plan: refuse to start if the list is empty, because then no line would be read as a tax. `MOCK_OCR` defaults to `true`, so one command runs the service. `DB_PATH` needs a file default, because `db.Open` refuses an empty path (the SQLite driver would ignore the connection settings in §3) and `:memory:` (each pooled connection would get its own empty database). The OCR timeout passed to `ocr.New` is a constant in `main.go`, e.g. 30 s.
