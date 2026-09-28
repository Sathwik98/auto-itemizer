# Receipt upload, taxes and auto-itemize

A small HTTP API that turns an uploaded receipt into one **transaction**. The transaction's **taxes** are stored as separate rows, and its **line items** are split out automatically (auto-itemize). If the items don't add up to the total, the transaction is kept and marked `NEEDS_REVIEW`: the service never invents a line to make the numbers work. You can then fix the items yourself with a PATCH.

**OCR is stubbed.** No OCR vendor or LLM is called. The service reads the text of the fixture whose name matches the uploaded file (see [How the OCR stub works](#how-the-ocr-stub-works)).

## Quick start

**Requirements:**
- **Go 1.25 or newer.** Check with `go version`. Go 1.21 to 1.24 downloads 1.25 by itself when it reads `go.mod`.
- **`curl` and `jq`, for the examples and the demo script.** jq comes with macOS 15 and later; elsewhere, run `brew install jq` or `sudo apt install jq`.
- **Nothing else.** You don't need a C compiler, a database server or Docker, because the SQLite driver is pure Go.

From the repository's root folder:

```bash
go run ./cmd/server
```

On the first start, the server:
- creates `storage/`, with the SQLite database `storage/auto-itemizer.db`;
- runs the database migrations: the tables, then the 16 known tax names;
- loads the fixtures from `fixtures/`;
- listens on http://localhost:8080. Only this machine can reach it, because the API has no login.

```
2026/09/28 10:10:22 INFO applied migration version=1 name=0001_create_tables
2026/09/28 10:10:22 INFO applied migration version=2 name=0002_seed_tax_names
2026/09/28 10:10:22 INFO listening url=http://localhost:8080 mock_ocr=true db=storage/auto-itemizer.db storage=storage
```

Then, in a second terminal, check everything in one go:

```bash
./scripts/demo.sh
```

It uploads every fixture, calls every endpoint and checks each answer. It prints one `ok` line per check, then `All 36 checks passed.`

To stop the server, press Ctrl-C: it finishes the requests in flight (up to 10 s) and exits. Press Ctrl-C again to quit at once.

Other commands:

```bash
go test ./...                    # all the tests; add -race to also check for data races
PORT=8081 go run ./cmd/server    # another port (then: BASE_URL=http://localhost:8081 ./scripts/demo.sh)
rm -rf storage                   # with the server stopped: start again from an empty database
```

## Walkthrough

Copy and paste these with the server running. They use `jq` to pick the ids out of the answers.

```bash
# 1. Upload a receipt. The answer has its id.
RECEIPT=$(curl -s -F "file=@fixtures/task-a/receipt-clean.txt" localhost:8080/receipts | jq -r .receipt_id)

# 2. Run OCR, extraction and auto-itemize. The answer is the new transaction.
TRANSACTION=$(curl -s -X POST localhost:8080/receipts/$RECEIPT/process | jq -r .id)

# 3. Read the transaction back.
curl -s localhost:8080/transactions/$TRANSACTION | jq
```

```json
{
  "id": "52c8338b-1cf1-442d-8e34-66c73101ada4",
  "receipt_id": "709f25e2-d0af-4860-b38b-4f6cb178e3f9",
  "merchant": "Cafe Mitte",
  "date": "2026-03-12",
  "currency": "EUR",
  "grand_total": "17.85",
  "taxes": [
    {
      "name": "VAT",
      "rate": "0.19",
      "taxable_amount": "15.00",
      "amount": "2.85"
    }
  ],
  "line_items": [
    {
      "id": "13e4fdc6-a626-48d4-ba78-60baae5a3c05",
      "description": "Espresso",
      "amount": "3.50"
    },
    {
      "id": "753b4ca3-ef94-4b3f-a69c-4b478f93d09e",
      "description": "Sandwich",
      "amount": "8.90"
    },
    {
      "id": "be388221-fb64-4728-83c2-931b528a0749",
      "description": "Mineral water",
      "amount": "2.60"
    }
  ],
  "itemize_status": "COMPLETE"
}
```

- **Money** is always a string with two decimals (`"17.85"`), so it's exact.
- **Rates** are exact fractions, never rounded: `"0.19"` is 19%, and `"0.09975"` is 9.975%.
- **`taxable_amount`** is the amount the tax was charged on. It's null when the receipt doesn't show it.

The brief's other two fixtures work the same way:

```bash
for f in receipt-tax-only receipt-mismatch; do
  R=$(curl -s -F "file=@fixtures/task-a/$f.txt" localhost:8080/receipts | jq -r .receipt_id)
  curl -s -X POST localhost:8080/receipts/$R/process | jq -c '{merchant, grand_total, items: [.line_items[].description], itemize_status}'
done
```

```
{"merchant":"Berlin Taxi GmbH","grand_total":"24.00","items":[],"itemize_status":"NEEDS_REVIEW"}
{"merchant":"Hotel Shop","grand_total":"18.50","items":["Water","Snacks"],"itemize_status":"NEEDS_REVIEW"}
```

- **`receipt-tax-only`** prints no items, so there is nothing to itemize.
- **`receipt-mismatch`'s** items (4.00 + 6.00, plus 1.90 VAT) don't reach its 18.50 total. The transaction keeps only the lines that are printed.

## API

The examples use `$RECEIPT` and `$TRANSACTION` from the walkthrough, in order.

- **Error format:** every error is JSON, `{"error": "<CODE>", "message": "…"}`. All the codes are listed in [ARCHITECTURE.md §3.0](ARCHITECTURE.md#30-common-behaviour).
- **Naming:** "transaction" is the brief's name for what the code calls an expense, as in `expense_id`.

### `GET /health`

```bash
curl -s localhost:8080/health
```
```json
{"status":"ok"}
```

### `POST /receipts`: upload

```bash
curl -si -F "file=@fixtures/task-a/receipt-clean.txt" localhost:8080/receipts
```
```
HTTP/1.1 201 Created
Content-Type: application/json
Location: /receipts/afff7616-e917-4532-9f2c-7ead24ef8b59
Date: Mon, 28 Sep 2026 04:40:26 GMT
Content-Length: 74

{"receipt_id":"afff7616-e917-4532-9f2c-7ead24ef8b59","status":"UPLOADED"}
```

- **What's accepted:** a multipart form with the file in the field `file`, of type `image/*`, `application/pdf` or `text/plain`, up to 10 MB.
- **File types:** the type is the one the client sends. curl picks it from the extension for `.txt`, `.png`, `.jpg`, `.gif` and `.pdf`. For other image types, name it: `-F "file=@receipt.heic;type=image/heic"`.
- **Where it goes:** the file is stored under `storage/receipts/`, with a new name.
- **Errors:**
  - 400: `FILE_MISSING`, `FILE_EMPTY`, `INVALID_UPLOAD` (not a multipart form);
  - 413: `FILE_TOO_LARGE`;
  - 415: `UNSUPPORTED_FILE_TYPE`.

### `POST /receipts/{id}/process`: OCR, extraction, taxes and auto-itemize

```bash
TRANSACTION=$(curl -s -X POST localhost:8080/receipts/$RECEIPT/process | jq -r .id)
```

- **Answer:** 200 with the new transaction, in the same shape as `GET /transactions/{id}`.
- **What it stores:**
  - the raw OCR text;
  - the header: merchant, date, currency and total;
  - one row per printed tax;
  - the auto-itemized line items.
- **Processing again**, as here after the walkthrough, replaces the receipt's transaction with a new one, with a new id. The command above keeps the new id; the old one now answers 404.
- **If the text isn't a usable receipt:** 422 with the reason, and the receipt is marked `FAILED`. The codes are `OCR_FAILED`, `OCR_UNREADABLE`, `NOT_A_RECEIPT`, `HEADER_INCOMPLETE` and `INVALID_RECEIPT_VALUES` ([ARCHITECTURE.md §1.3](ARCHITECTURE.md#13-guards)).

```bash
R=$(curl -s -F "file=@fixtures/mock-ocr/not-a-receipt.txt" localhost:8080/receipts | jq -r .receipt_id)
curl -s -X POST localhost:8080/receipts/$R/process
```
```json
{"error":"NOT_A_RECEIPT","message":"no total line (TOTAL, SUMME, GESAMT or AMOUNT DUE) with an amount"}
```

- **Other errors:**
  - 404 for an unknown receipt;
  - 409 `CONFLICT` if another request changed the receipt at the same time;
  - 501 `LIVE_OCR_NOT_CONFIGURED` when started with `MOCK_OCR=false`.

### `GET /receipts/{id}`: receipt status

```bash
curl -s localhost:8080/receipts/$RECEIPT | jq
```
```json
{
  "id": "709f25e2-d0af-4860-b38b-4f6cb178e3f9",
  "status": "PROCESSED",
  "failure_reason": null,
  "file": {
    "file_name": "receipt-clean.txt",
    "content_type": "text/plain",
    "size_bytes": 252
  },
  "expense_id": "d798af5d-5f0a-4ba3-aba6-65dc9cda6af7",
  "created_at": "2026-09-28T04:40:26.306426Z",
  "updated_at": "2026-09-28T04:40:26.367508Z"
}
```

- **`status`** is one of `UPLOADED`, `OCR_EXTRACTED`, `PROCESSED` or `FAILED`.
- **`failure_reason`** is the error code of a `FAILED` receipt.
- **`expense_id`** is the id of its current transaction, or null when it has none.

### `GET /transactions/{id}`: the transaction, its taxes and line items

```bash
curl -s localhost:8080/transactions/$TRANSACTION | jq
```

200 with the shape shown in the walkthrough; 404 `NOT_FOUND` for an unknown or replaced transaction.

### `POST /transactions/{id}/itemize`: re-run auto-itemize

```bash
curl -s -X POST localhost:8080/transactions/$TRANSACTION/itemize | jq -c '{itemize_status, items: [.line_items[] | "\(.description) \(.amount)"]}'
```
```
{"itemize_status":"COMPLETE","items":["Espresso 3.50","Sandwich 8.90","Mineral water 2.60"]}
```

- **What it does:** it re-runs auto-itemize from the **stored** OCR text and replaces the line items, which get new ids.
- **What stays:** the header, the taxes and the transaction id. No second transaction is created.
- **Your PATCH edits are undone:** it goes back to the automatic result.

### `PATCH /transactions/{id}/items`: edit, merge or split items

The body is the **complete** list of items you want:
- **Unchanged items:** an item sent with its `id` and an unchanged description and amount keeps that id.
- **Changed or new items:** an item you changed, or one without an `id`, is stored as a new item with a new id, listed after the unchanged ones.
- **Removed items:** an item you leave out is removed.
- **Split and merge:** a split is one item replaced by two, and a merge is two replaced by one.
- **Amounts:** `amount` can be `"6.60"` or `6.60`.

On `receipt-mismatch`, sending only Water and Snacks is refused, because they don't reach the total:

```bash
R=$(curl -s -F "file=@fixtures/task-a/receipt-mismatch.txt" localhost:8080/receipts | jq -r .receipt_id)
T=$(curl -s -X POST localhost:8080/receipts/$R/process | jq -r .id)
WATER=$(curl -s localhost:8080/transactions/$T | jq -r '.line_items[0].id')
SNACKS=$(curl -s localhost:8080/transactions/$T | jq -r '.line_items[1].id')

curl -s -X PATCH localhost:8080/transactions/$T/items -H 'Content-Type: application/json' \
  -d "[{\"id\":\"$WATER\",\"description\":\"Water\",\"amount\":\"4.00\"},
       {\"id\":\"$SNACKS\",\"description\":\"Snacks\",\"amount\":\"6.00\"}]"
```
```json
{"error":"ITEMS_DO_NOT_RECONCILE","message":"Items plus taxes do not equal the total","expected":"18.50","actual":"11.90","difference":"6.60"}
```

The service never changes the total or the taxes. Adding the missing 6.60 yourself reconciles, and the transaction becomes `COMPLETE`:

```bash
curl -s -X PATCH localhost:8080/transactions/$T/items -H 'Content-Type: application/json' \
  -d "[{\"id\":\"$WATER\",\"description\":\"Water\",\"amount\":\"4.00\"},
       {\"id\":\"$SNACKS\",\"description\":\"Snacks\",\"amount\":\"6.00\"},
       {\"description\":\"Minibar\",\"amount\":\"6.60\"}]" | jq -c '{itemize_status, items: [.line_items[] | "\(.description) \(.amount)"]}'
```
```
{"itemize_status":"COMPLETE","items":["Water 4.00","Snacks 6.00","Minibar 6.60"]}
```

- **Refused bodies:**
  - 400 `INVALID_BODY`: not a JSON array, or an unknown field such as `tax_amount`;
  - 400 `INVALID_ITEM`: an empty or blank description, an amount of 0 or with more than two decimals, or a repeated id;
  - 400 `UNKNOWN_ITEM`: an id that isn't an item of this transaction.
- **More examples:** `scripts/demo.sh` has a split, a merge and an edit.

### `GET /metrics`: Prometheus metrics

```bash
curl -s localhost:8080/metrics | grep '^receipt_outcomes_total'
```
```
receipt_outcomes_total{outcome="COMPLETE"} 2
receipt_outcomes_total{outcome="HEADER_INCOMPLETE"} 0
receipt_outcomes_total{outcome="INVALID_RECEIPT_VALUES"} 0
receipt_outcomes_total{outcome="NEEDS_REVIEW"} 3
receipt_outcomes_total{outcome="NOT_A_RECEIPT"} 1
receipt_outcomes_total{outcome="OCR_FAILED"} 0
receipt_outcomes_total{outcome="OCR_UNREADABLE"} 0
```

The counts depend on what you've run. See [Logs and metrics](#logs-and-metrics).

## Fixtures

Upload a fixture file, or any file with the same name and another extension (e.g. `receipt-clean.png`), to get that fixture's text.

| File | What it is | Result of process |
|---|---|---|
| `fixtures/task-a/receipt-clean.txt` | The brief's happy path: 3 items + VAT | 200 `COMPLETE`, 3 items, VAT 2.85 on 15.00 |
| `fixtures/task-a/receipt-tax-only.txt` | The brief's taxi receipt: VAT included, no items | 200 `NEEDS_REVIEW`, no items |
| `fixtures/task-a/receipt-mismatch.txt` | The brief's mismatch: items don't reach the total | 200 `NEEDS_REVIEW`, Water + Snacks, no invented line |
| `fixtures/mock-ocr/unreadable.txt` | Symbols, too few letters or digits | 422 `OCR_UNREADABLE` |
| `fixtures/mock-ocr/not-a-receipt.txt` | A letter, with no total | 422 `NOT_A_RECEIPT` |
| `fixtures/mock-ocr/header-incomplete.txt` | A total, but no merchant or date | 422 `HEADER_INCOMPLETE` |
| `fixtures/mock-ocr/invalid-values.txt` | A total of 0.00 | 422 `INVALID_RECEIPT_VALUES` |
| `fixtures/mock-ocr/receipt-gst-qst.txt` | Canada: GST 5% + QST 9.975% on one subtotal | 200 `COMPLETE`, two tax rows |
| `fixtures/mock-ocr/receipt-cgst-sgst.txt` | India: CGST 9% + SGST 9%, INR | 200 `COMPLETE`, two tax rows |
| `fixtures/mock-ocr/receipt-discount.txt` | A `Discount 10% -1.40` line | 200 `COMPLETE`, the discount as an item |
| any other name, e.g. `photo.png` | No fixture matches | the text of `receipt-clean` |

- **Gold answers:** the results for the first three match `fixtures/task-a/gold.json`.
- **Folders:** `fixtures/task-a/` is the brief's folder, unchanged; `fixtures/mock-ocr/` holds our extras.

## How the OCR stub works

- **Matching by name:** the mock OCR takes the uploaded file's name without its extension (`receipt-clean.png` → `receipt-clean`). It returns the fixture text with that name from `fixtures/task-a/` or `fixtures/mock-ocr/`.
- **Fallback:** any other name gets the text of `fixtures/task-a/receipt-clean.txt`. The server logs that it fell back and counts it in `mock_ocr_fallbacks_total`.
- **The file itself is never read.** A real OCR provider would plug in behind the same interface.
- **`MOCK_OCR=false`:** this selects that "live" provider. It isn't configured, so `process` answers 501 without writing anything.

After OCR, the service checks that the text is a usable receipt (the guards). It then parses the text into the header, taxes and candidate lines, and auto-itemizes. The rules are in [ARCHITECTURE.md §1.3 and §4](ARCHITECTURE.md).

## Settings

All are optional environment variables. A bad value stops the server with a message naming the variable.

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | Port to listen on, on `127.0.0.1` only |
| `STORAGE_DIR` | `storage` | Uploaded files go to `<STORAGE_DIR>/receipts/` |
| `DB_PATH` | `<STORAGE_DIR>/auto-itemizer.db` | The SQLite database file |
| `FIXTURES_DIR` | `fixtures` | Where the mock OCR finds `task-a/` and `mock-ocr/` |
| `MOCK_OCR` | `true` | `false` selects the live OCR provider, which answers 501 |
| `MOCK_OCR_FALLBACK` | `<FIXTURES_DIR>/task-a/receipt-clean.txt` | The text for an upload whose name matches no fixture |

## Logs and metrics

**Logs** go to standard error. There is one line per request, plus one per event, such as a processed receipt or a refused PATCH:

```
2026/09/28 10:10:26 INFO request method=POST path=/receipts route="POST /receipts" status=201 duration_ms=0
2026/09/28 10:10:26 INFO receipt processed receipt_id=709f25e2-d0af-4860-b38b-4f6cb178e3f9 transaction_id=52c8338b-1cf1-442d-8e34-66c73101ada4 itemize_status=COMPLETE
2026/09/28 10:10:26 INFO request method=POST path=/receipts/709f25e2-d0af-4860-b38b-4f6cb178e3f9/process route="POST /receipts/{id}/process" status=200 duration_ms=0
…
2026/09/28 10:10:26 WARN receipt failed a guard receipt_id=e749dd79-c8a9-4428-b3a7-e26d702c2166 code=NOT_A_RECEIPT reason="no total line (TOTAL, SUMME, GESAMT or AMOUNT DUE) with an amount"
2026/09/28 10:10:26 WARN request method=POST path=/receipts/e749dd79-c8a9-4428-b3a7-e26d702c2166/process route="POST /receipts/{id}/process" status=422 duration_ms=0
…
2026/09/28 10:10:26 WARN PATCH refused: the items don't add up transaction_id=bc38e33b-b0a8-4f36-a162-3284637a28da expected=18.50 actual=11.90 difference=6.60
2026/09/28 10:10:26 WARN request method=PATCH path=/transactions/bc38e33b-b0a8-4f36-a162-3284637a28da/items route="PATCH /transactions/{id}/items" status=409 duration_ms=0
```

The level follows the status: INFO for successes, WARN for 4xx, ERROR for 5xx.

**Metrics** are at `GET /metrics`, in the Prometheus format:

| Metric | What it counts |
|---|---|
| `http_requests_total{route,status}`, `http_request_duration_seconds{route}` | Requests and their durations, by route pattern (`unmatched` for unknown paths) |
| `receipt_outcomes_total{outcome}` | Processed receipts: `COMPLETE`, `NEEDS_REVIEW`, or the code of the failed check |
| `mock_ocr_fallbacks_total` | Uploads whose name matched no fixture |
| `patches_refused_total` | PATCHes refused because the items don't add up |
| `write_conflicts_total`, `internal_errors_total`, `panics_total` | 409 `CONFLICT`s, unexpected 500s and handler panics |

To graph them, point Prometheus at the service with this `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: auto-itemizer
    static_configs:
      - targets: ["localhost:8080"]
```

- **Run it:** `prometheus --config.file=prometheus.yml` (on macOS, install it with `brew install prometheus`).
- **Graph it:** add Prometheus as a data source in Grafana. Both are free and open source, and Grafana Cloud's free tier also works.

## Database and migrations

- **Where it is:** the database is one SQLite file, `storage/auto-itemizer.db` (`DB_PATH`).
- **Migrations run by themselves.** The schema lives in numbered SQL files in `internal/db/migrations/`, built into the binary.
  - At startup, the server applies the ones the database hasn't had yet, in number order, and records each in the `schema_migrations` table.
  - A failing migration stops startup with the file's name, and none of that file is applied.
- **To change the schema,** add the next file, e.g. `internal/db/migrations/0003_add_note_column.sql`, and restart. Never edit a file that has already run.
- **To start again from empty,** stop the server and run `rm -rf storage`.

## Tests

```bash
go test ./...
```

- **Parser and rules:** each fixture's header, taxes and items are compared with `gold.json`, and each guard fixture must give its code.
- **Services:** run on a temporary database. This includes an OCR failure or timeout (`OCR_FAILED`), which the mock OCR can't produce.
- **HTTP API:** end to end, covering every endpoint, every fixture, the error codes, and the edit, merge and split PATCHes.
- **Startup and shutdown:** these include a request that's still running when the server stops, and a second Ctrl-C that quits at once.
- **Also:**
  - the migrations;
  - the logs and metrics;
  - a layout test that keeps each feature's SQL private to that feature.

`./scripts/demo.sh` then checks the running service over real HTTP.

## Troubleshooting

- **`listen tcp 127.0.0.1:8080: bind: address already in use`:** another program uses port 8080. Use `PORT=8081 go run ./cmd/server`.
- **`read mock OCR folder: … (run from the repo root, or set FIXTURES_DIR)`:** run the commands from the repository's root folder.
- **`go.mod requires go >= 1.25.0`, or `invalid go version`:** your Go is too old to fetch 1.25 by itself (before 1.21), or `GOTOOLCHAIN=local` is set. Install Go 1.25 or newer from https://go.dev/dl/.
- **Can't reach it from another machine or a container:** that's by design. The server only listens on `127.0.0.1`, because the API has no login.

## Project layout

```
cmd/server/             the program: settings, wiring, startup and shutdown
internal/
  receipt/  expense/  fileupload/
      server/ service/ core/ repository/   one folder per layer, per feature
  httpapi/              the router; models/ (JSON bodies) and respond/ (errors)
  ocr/                  the OCR service: the mock and "live" providers
  db/                   the SQLite connection, transactions and migrations/
  metrics/  config/  layout/
fixtures/               task-a/ (the brief's, unchanged) and mock-ocr/ (ours)
scripts/demo.sh         the end-to-end check
```

- [ARCHITECTURE.md](ARCHITECTURE.md): how the service behaves. It covers the components, the data model, each endpoint step by step, and the itemize and reconcile rules.
- [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md): how it's built. It covers the stack, the folder rules, the storage details, the tests, and the plan behind each component.
