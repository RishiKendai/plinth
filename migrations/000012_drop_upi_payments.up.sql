-- The Pay with UPI feature was removed (owner decision 2026-11-20, docs/capture-rules.md). Nothing reads or writes this table any more.
-- Approved by the owner. Existing transactions that came from it keep source = 'upi' and are untouched; only the attempt records go.
DROP TABLE IF EXISTS upi_payments;
