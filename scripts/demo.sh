#!/usr/bin/env bash
# Runs every fixture and endpoint of the receipt API against a running server
# and checks each answer. Start the server first, from the repo root:
#
#   go run ./cmd/server
#
# then run this in a second terminal:  ./scripts/demo.sh
# BASE_URL changes the server address (default http://localhost:8080).
# The exit code is 1 if any check fails, so it also works as a smoke test.
set -uo pipefail

BASE="${BASE_URL:-http://localhost:8080}"
cd "$(dirname "$0")/.." # the fixture paths below are relative to the repo root

if ! command -v jq >/dev/null 2>&1; then
  echo "This script needs jq: brew install jq (macOS) or sudo apt install jq (Debian/Ubuntu)." >&2
  exit 1
fi
if ! curl -sf "$BASE/health" >/dev/null; then
  echo "No server answers at $BASE. Start it from the repo root with: go run ./cmd/server" >&2
  exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
passed=0
failed=0

# check LABEL GOT WANT prints one ok or FAIL line.
check() {
  if [ "$2" = "$3" ]; then
    passed=$((passed + 1))
    printf '  ok    %s\n' "$1"
  else
    failed=$((failed + 1))
    printf '  FAIL  %s\n        got:  %s\n        want: %s\n' "$1" "$2" "$3"
  fi
}

# request METHOD PATH [curl options...] sets STATUS and BODY.
request() {
  local method=$1 path=$2 out
  shift 2
  out=$(curl -s -X "$method" -w $'\n%{http_code}' "$@" "$BASE$path")
  STATUS=${out##*$'\n'}
  BODY=${out%$'\n'*}
}

# field JQ-FILTER reads a value from the last answer. An error answer has no
# such field, so jq's complaint is dropped and the check shows what was read.
field() { jq -r "$1" <<<"$BODY" 2>/dev/null; }

# upload FILE sets RECEIPT to the new receipt's id.
upload() {
  request POST /receipts -F "file=@$1"
  RECEIPT=$(jq -r '.receipt_id // empty' <<<"$BODY")
}

# process FILE uploads and processes it, and sets TRANSACTION.
process() {
  upload "$1"
  request POST "/receipts/$RECEIPT/process"
  TRANSACTION=$(jq -r '.id // empty' <<<"$BODY")
}

# patch TRANSACTION JSON sends a PATCH /transactions/{id}/items.
patch() {
  request PATCH "/transactions/$1/items" -H 'Content-Type: application/json' -d "$2"
}

items='[.line_items[].description] | join(", ")'

echo "Receipt API demo against $BASE"

echo
echo "1. The brief's fixtures (fixtures/task-a): upload, process, read back"
process fixtures/task-a/receipt-clean.txt
check "receipt-clean: process → 200" "$STATUS" 200
check "receipt-clean: merchant, total, status" "$(field '"\(.merchant), \(.grand_total), \(.itemize_status)"')" "Cafe Mitte, 17.85, COMPLETE"
check "receipt-clean: the tax as printed" "$(field '.taxes[0] | "\(.name) \(.rate) on \(.taxable_amount) = \(.amount)"')" "VAT 0.19 on 15.00 = 2.85"
check "receipt-clean: the items" "$(field "$items")" "Espresso, Sandwich, Mineral water"
CLEAN_RECEIPT=$RECEIPT
CLEAN=$TRANSACTION
request GET "/transactions/$CLEAN"
check "GET /transactions/{id} → the same transaction" "$STATUS $(field .grand_total)" "200 17.85"
request GET "/receipts/$CLEAN_RECEIPT"
check "GET /receipts/{id} → PROCESSED, with its transaction" "$(field '"\(.status) \(.expense_id)"')" "PROCESSED $CLEAN"

process fixtures/task-a/receipt-tax-only.txt
check "receipt-tax-only: total, items, status" "$(field '"\(.grand_total), \(.line_items | length) items, \(.itemize_status)"')" "24.00, 0 items, NEEDS_REVIEW"

process fixtures/task-a/receipt-mismatch.txt
check "receipt-mismatch: total, items, status (no invented line)" "$(field '"\(.grand_total), \([.line_items[].description] | join(" + ")), \(.itemize_status)"')" "18.50, Water + Snacks, NEEDS_REVIEW"
MISMATCH=$TRANSACTION
WATER=$(field '.line_items[0].id')
SNACKS=$(field '.line_items[1].id')

echo
echo "2. Guard fixtures (fixtures/mock-ocr): refused with 422, the receipt marked FAILED"
for pair in unreadable:OCR_UNREADABLE not-a-receipt:NOT_A_RECEIPT header-incomplete:HEADER_INCOMPLETE invalid-values:INVALID_RECEIPT_VALUES; do
  name=${pair%%:*}
  code=${pair#*:}
  process "fixtures/mock-ocr/$name.txt"
  check "$name → 422 $code" "$STATUS $(field .error)" "422 $code"
  request GET "/receipts/$RECEIPT"
  check "$name: receipt status and reason" "$(field '"\(.status) \(.failure_reason)"')" "FAILED $code"
done

echo
echo "3. Extra receipts (fixtures/mock-ocr)"
process fixtures/mock-ocr/receipt-gst-qst.txt
check "receipt-gst-qst: two taxes on one subtotal" "$(field '"\([.taxes[] | "\(.name) \(.rate)"] | join(" + ")), \(.itemize_status)"')" "GST 0.05 + QST 0.09975, COMPLETE"
process fixtures/mock-ocr/receipt-cgst-sgst.txt
check "receipt-cgst-sgst: Indian GST" "$(field '"\([.taxes[].name] | join(" + ")), \(.currency) \(.grand_total), \(.itemize_status)"')" "CGST + SGST, INR 212.40, COMPLETE"
process fixtures/mock-ocr/receipt-discount.txt
check "receipt-discount: a discount line item" "$(field '"\(.line_items[2].description) \(.line_items[2].amount), \(.itemize_status)"')" "Discount 10% -1.40, COMPLETE"

echo
echo "4. A file name that matches no fixture uses receipt-clean's text"
printf 'not a real image' >"$tmp/photo.png"
process "$tmp/photo.png"
check "photo.png → the fallback receipt" "$(field '"\(.merchant), \(.itemize_status)"')" "Cafe Mitte, COMPLETE"

echo
echo "5. PATCH and re-itemize on receipt-mismatch"
patch "$MISMATCH" "[{\"id\":\"$WATER\",\"description\":\"Water\",\"amount\":\"4.00\"},{\"id\":\"$SNACKS\",\"description\":\"Snacks\",\"amount\":\"6.00\"}]"
check "only Water + Snacks → 409 with the numbers" "$STATUS $(field '"\(.error) \(.expected) \(.actual) \(.difference)"')" "409 ITEMS_DO_NOT_RECONCILE 18.50 11.90 6.60"
patch "$MISMATCH" "[{\"id\":\"$WATER\",\"description\":\"Water\",\"amount\":\"4.00\"},{\"id\":\"$SNACKS\",\"description\":\"Snacks\",\"amount\":\"6.00\"},{\"description\":\"Minibar\",\"amount\":\"6.60\"}]"
check "add the missing Minibar 6.60 → 200 COMPLETE" "$STATUS $(field .itemize_status)" "200 COMPLETE"
request POST "/transactions/$MISMATCH/itemize"
check "re-itemize → back to the automatic NEEDS_REVIEW" "$STATUS $(field .itemize_status)" "200 NEEDS_REVIEW"

echo
echo "6. Split, merge and edit on receipt-clean"
request GET "/transactions/$CLEAN"
ESPRESSO=$(field '.line_items[0].id')
WATER=$(field '.line_items[2].id')
patch "$CLEAN" "[{\"id\":\"$ESPRESSO\",\"description\":\"Espresso\",\"amount\":\"3.50\"},{\"description\":\"Bread\",\"amount\":\"5.00\"},{\"description\":\"Cheese\",\"amount\":\"3.90\"},{\"id\":\"$WATER\",\"description\":\"Mineral water\",\"amount\":\"2.60\"}]"
check "split Sandwich 8.90 into Bread 5.00 + Cheese 3.90" "$STATUS $(field "$items")" "200 Espresso, Mineral water, Bread, Cheese"
ESPRESSO=$(field '.line_items[0].id')
WATER=$(field '.line_items[1].id')
patch "$CLEAN" "[{\"id\":\"$ESPRESSO\",\"description\":\"Espresso\",\"amount\":\"3.50\"},{\"id\":\"$WATER\",\"description\":\"Mineral water\",\"amount\":\"2.60\"},{\"description\":\"Sandwich\",\"amount\":\"8.90\"}]"
check "merge Bread + Cheese back into Sandwich" "$STATUS $(field "$items")" "200 Espresso, Mineral water, Sandwich"
SANDWICH=$(field '.line_items[2].id')
patch "$CLEAN" "[{\"id\":\"$ESPRESSO\",\"description\":\"Espresso\",\"amount\":\"3.00\"},{\"id\":\"$WATER\",\"description\":\"Mineral water\",\"amount\":\"3.10\"},{\"id\":\"$SANDWICH\",\"description\":\"Sandwich\",\"amount\":\"8.90\"}]"
check "edit: move 0.50 from Espresso to Mineral water" "$STATUS $(field '[.line_items[] | "\(.description) \(.amount)"] | join(", ")')" "200 Sandwich 8.90, Espresso 3.00, Mineral water 3.10"

echo
echo "7. Reprocess: a new transaction replaces the old one"
request POST "/receipts/$CLEAN_RECEIPT/process"
NEW=$(field .id)
check "reprocess → a new transaction id" "$([ -n "$NEW" ] && [ "$NEW" != "$CLEAN" ] && echo new || echo same)" "new"
request GET "/transactions/$CLEAN"
check "the old transaction → 404" "$STATUS $(field .error)" "404 NOT_FOUND"

echo
echo "8. Errors"
request GET /transactions/no-such-id
check "unknown transaction → 404" "$STATUS $(field .error)" "404 NOT_FOUND"
request GET /no/such/path
check "unknown path → 404" "$STATUS $(field .error)" "404 NOT_FOUND"
request DELETE "/receipts/$CLEAN_RECEIPT"
check "wrong method → 405" "$STATUS $(field .error)" "405 METHOD_NOT_ALLOWED"
: >"$tmp/empty.txt"
upload "$tmp/empty.txt"
check "empty file → 400" "$STATUS $(field .error)" "400 FILE_EMPTY"
printf 'PK' >"$tmp/receipt.zip"
upload "$tmp/receipt.zip"
check "zip file → 415" "$STATUS $(field .error)" "415 UNSUPPORTED_FILE_TYPE"
request POST /receipts -H 'Content-Type: application/json' -d '{}'
check "not a multipart form → 400" "$STATUS $(field .error)" "400 INVALID_UPLOAD"
patch "$MISMATCH" '[{"description":'
check "broken PATCH JSON → 400" "$STATUS $(field .error)" "400 INVALID_BODY"

echo
echo "9. Metrics"
outcomes=$(curl -s "$BASE/metrics" | grep -c '^receipt_outcomes_total{')
check "GET /metrics lists all 7 receipt outcomes" "$outcomes" 7

echo
if [ "$failed" -eq 0 ]; then
  printf 'All %d checks passed.\n' "$passed"
else
  printf '%d checks passed, %d failed.\n' "$passed" "$failed"
  exit 1
fi
