-- A group expense made from one of the member's own transactions takes that transaction's date, so a group lists it in time order.
-- Before, it was dated by when it was put in the group.
UPDATE group_expenses e SET occurred_at = t.occurred_at
  FROM transactions t
 WHERE e.transaction_id = t.id AND e.occurred_at <> t.occurred_at;
