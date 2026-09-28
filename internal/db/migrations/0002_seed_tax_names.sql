-- Migration 0002: the tax names known before any receipt is processed.
--
-- They are also the names the parser uses to recognise tax lines. They are
-- loaded once at startup, so a new name needs a new migration and a restart.
-- SQL can't generate UUIDs, so the ids are fixed. ON CONFLICT DO NOTHING lets
-- this adopt a database created before migrations existed.
INSERT INTO tax_master (id, name, created_at, created_by, updated_at, updated_by) VALUES
    ('33cc20c4-5b41-43ed-9003-0f285570ebd9', 'VAT',       '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('c2d655ba-b323-4582-98db-fd3a9106ccd8', 'GST',       '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('2ebfae49-30d6-48a4-9e37-3f5840145d7f', 'HST',       '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('83a21ee1-b351-4eb5-863d-5d2d9f26e5bf', 'PST',       '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('5d34f725-77d2-493e-9f73-963d9b1777de', 'QST',       '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('a3ba6bc2-2a57-48c8-84a1-2d04dbd8a642', 'MWST',      '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('67f715d4-ed13-4433-8ab0-caf09379ccf8', 'UST',       '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('8fa63dea-77f4-47b3-971d-54acc0f95a25', 'TVA',       '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('49fd1209-8738-4542-a047-05b64b704161', 'IVA',       '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('527789c0-ab59-4019-81f0-10175e31bf17', 'TAX',       '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('7550cb53-abc7-4c1b-8e9e-0296a4fccc9f', 'SALES TAX', '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('476bb6af-62dd-4bc7-b7d7-ad8ceaad28e3', 'CGST',      '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('742f7b1a-25c7-451d-86d1-ebc593b57db2', 'SGST',      '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('bdc6bc37-f86f-4c16-b6e7-b7b3b83e84d0', 'IGST',      '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('baeed473-7f91-4edd-b49d-c6c44fe7c6fb', 'UTGST',     '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system'),
    ('27c29a77-6c55-43a5-bbf7-e67fabc4b172', 'CESS',      '2026-09-27T00:00:00.000000Z', 'system', '2026-09-27T00:00:00.000000Z', 'system')
ON CONFLICT (name) DO NOTHING;
