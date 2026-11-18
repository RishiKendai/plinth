DROP TABLE IF EXISTS upi_payments;
DROP TABLE IF EXISTS sms_events;
DROP TABLE IF EXISTS import_rows;
DROP TABLE IF EXISTS import_batches;
DROP TABLE IF EXISTS attachments;
ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_counted_amount_check;
ALTER TABLE transactions DROP COLUMN IF EXISTS counted_amount;
