DROP TABLE IF EXISTS group_topups;
ALTER TABLE transactions DROP CONSTRAINT transactions_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_source_check CHECK (source IN
    ('manual', 'upi', 'sms', 'csv', 'recurring', 'group', 'receipt', 'reconcile', 'settlement'));
