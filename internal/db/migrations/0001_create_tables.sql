-- Migration 0001: the tables and indexes of ARCHITECTURE.md §2.
--
-- Migrations run once each, in number order, when the service starts
-- (internal/db/migrate.go), and each is recorded in schema_migrations. Never
-- edit or delete a migration that has run: add the next number instead.
-- IF NOT EXISTS lets this adopt a database created before migrations existed.
--
-- Storage types (IMPLEMENTATION_PLAN.md §3):
--   ids         TEXT, UUID
--   timestamps  TEXT, UTC, fixed width: 2006-01-02T15:04:05.000000Z
--   money/rates TEXT, decimal strings such as '17.85' and '0.1900'
--   booleans    INTEGER, 0 or 1
--   dates       TEXT, YYYY-MM-DD

CREATE TABLE IF NOT EXISTS file_upload (
    id           TEXT    PRIMARY KEY,
    file_path    TEXT    NOT NULL,
    file_name    TEXT    NOT NULL,
    content_type TEXT    NOT NULL,
    size_bytes   INTEGER NOT NULL,
    created_at   TEXT    NOT NULL,
    created_by   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL,
    updated_by   TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS receipts (
    id             TEXT PRIMARY KEY,
    file_id        TEXT NOT NULL UNIQUE REFERENCES file_upload (id),
    status         TEXT NOT NULL
                   CHECK (status IN ('UPLOADED', 'OCR_EXTRACTED', 'PROCESSED', 'FAILED')),
    failure_reason TEXT,
    created_at     TEXT NOT NULL,
    created_by     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    updated_by     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS receipt_ocr (
    id          TEXT    PRIMARY KEY,
    receipt_id  TEXT    NOT NULL REFERENCES receipts (id),
    ocr_payload TEXT    NOT NULL,
    is_deleted  INTEGER NOT NULL DEFAULT 0 CHECK (is_deleted IN (0, 1)),
    created_at  TEXT    NOT NULL,
    created_by  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL,
    updated_by  TEXT    NOT NULL
);

-- One active OCR run per receipt.
CREATE UNIQUE INDEX IF NOT EXISTS receipt_ocr_one_active
    ON receipt_ocr (receipt_id) WHERE is_deleted = 0;

CREATE TABLE IF NOT EXISTS expense (
    id                 TEXT    PRIMARY KEY,
    receipt_id         TEXT    NOT NULL REFERENCES receipts (id),
    itemization_status TEXT    NOT NULL
                       CHECK (itemization_status IN ('COMPLETE', 'NEEDS_REVIEW', 'FAILED')),
    merchant           TEXT    NOT NULL,
    date               TEXT    NOT NULL,
    currency           TEXT    NOT NULL,
    total              TEXT    NOT NULL,
    is_deleted         INTEGER NOT NULL DEFAULT 0 CHECK (is_deleted IN (0, 1)),
    created_at         TEXT    NOT NULL,
    created_by         TEXT    NOT NULL,
    updated_at         TEXT    NOT NULL,
    updated_by         TEXT    NOT NULL
);

-- One active expense per receipt.
CREATE UNIQUE INDEX IF NOT EXISTS expense_one_active
    ON expense (receipt_id) WHERE is_deleted = 0;

CREATE TABLE IF NOT EXISTS tax_master (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS expense_tax (
    id             TEXT PRIMARY KEY,
    expense_id     TEXT NOT NULL REFERENCES expense (id),
    tax_master_id  TEXT NOT NULL REFERENCES tax_master (id),
    rate           TEXT NOT NULL,
    taxable_amount TEXT,
    tax_amount     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    created_by     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    updated_by     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS expense_tax_by_expense ON expense_tax (expense_id);

-- Items are returned in insertion order (ORDER BY rowid), so this must stay
-- an ordinary rowid table.
CREATE TABLE IF NOT EXISTS expense_line_item (
    id         TEXT    PRIMARY KEY,
    expense_id TEXT    NOT NULL REFERENCES expense (id),
    name       TEXT    NOT NULL,
    amount     TEXT    NOT NULL,
    is_deleted INTEGER NOT NULL DEFAULT 0 CHECK (is_deleted IN (0, 1)),
    created_at TEXT    NOT NULL,
    created_by TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,
    updated_by TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS expense_line_item_by_expense ON expense_line_item (expense_id);
