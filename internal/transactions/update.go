package transactions

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"plinth/server/internal/idempotency"
	"plinth/server/internal/ledger"
	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/ids"
)

// UpdateInput carries optional changes; nil means unchanged. Version is required: an edit made against a stale
// copy is rejected with 409 instead of silently overwriting (CLAUDE.md 6.4, no blind last-write-wins).
type UpdateInput struct {
	Version int `json:"version"`
	// Financial fields. Changing any of them on a pending or confirmed transaction reverses the original and
	// records a linked replacement; the original stays in history.
	AccountID  *string    `json:"account_id"`
	Type       *string    `json:"type"`
	Amount     *int64     `json:"amount"`
	OccurredAt *time.Time `json:"occurred_at"`
	// Descriptive fields, edited in place. An empty category_id clears the category.
	Title         *string   `json:"title"`
	Description   *string   `json:"description"`
	CategoryID    *string   `json:"category_id"`
	PaymentMethod *string   `json:"payment_method"`
	PayerName     *string   `json:"payer_name"`
	PayeeName     *string   `json:"payee_name"`
	Notes         *string   `json:"notes"`
	Location      *string   `json:"location"`
	Timezone      *string   `json:"timezone"`
	Tags          *[]string `json:"tags"`
}

func (in UpdateInput) financial(t Transaction) bool {
	return in.AccountID != nil && ids.Normalize(*in.AccountID) != t.AccountID ||
		in.Type != nil && *in.Type != t.Type ||
		in.Amount != nil && *in.Amount != t.Amount ||
		in.OccurredAt != nil && !in.OccurredAt.UTC().Equal(t.OccurredAt)
}

// merge applies the changes to a copy of the stored transaction.
func (in UpdateInput) merge(t Transaction) NewTx {
	n := NewTx{ID: t.ID, AccountID: t.AccountID, Type: t.Type, Amount: t.Amount, Title: t.Title, Description: t.Description,
		CategoryID: t.CategoryID, PaymentMethod: t.PaymentMethod, PayerName: t.PayerName, PayeeName: t.PayeeName, Status: t.Status,
		OccurredAt: t.OccurredAt, PostedAt: t.PostedAt, Timezone: t.Timezone, Notes: t.Notes, Location: t.Location,
		Latitude: t.Latitude, Longitude: t.Longitude, Source: t.Source, ExternalRef: t.ExternalRef, TransferID: t.TransferID,
		TransferDir: t.TransferDirection, RefundOf: t.RefundOf, Tags: t.Tags, CountedAmount: t.CountedAmount, HistoryOnly: t.HistoryOnly}
	if in.AccountID != nil {
		n.AccountID = ids.Normalize(*in.AccountID)
	}
	if in.Type != nil {
		n.Type = *in.Type
	}
	if in.Amount != nil {
		n.Amount = *in.Amount
	}
	if in.OccurredAt != nil {
		n.OccurredAt = in.OccurredAt.UTC()
	}
	if in.Title != nil {
		n.Title = *in.Title
	}
	if in.Description != nil {
		n.Description = *in.Description
	}
	if in.CategoryID != nil {
		if *in.CategoryID == "" {
			n.CategoryID = nil
		} else {
			n.CategoryID = in.CategoryID
		}
	}
	if in.PaymentMethod != nil {
		n.PaymentMethod = *in.PaymentMethod
	}
	if in.PayerName != nil {
		n.PayerName = *in.PayerName
	}
	if in.PayeeName != nil {
		n.PayeeName = *in.PayeeName
	}
	if in.Notes != nil {
		n.Notes = *in.Notes
	}
	if in.Location != nil {
		n.Location = *in.Location
	}
	if in.Timezone != nil {
		n.Timezone = *in.Timezone
	}
	if in.Tags != nil {
		n.Tags = *in.Tags
	}
	return n
}

// Update edits a transaction. Descriptive fields change in place. A change to amount, account, date or type on
// a pending or confirmed transaction is recorded as reverse-and-replace so the ledger stays explainable.
// The Idempotency-Key is optional here because the version check already makes a retry safe.
func (s *Service) Update(ctx context.Context, userID, key, id string, in UpdateInput) (Transaction, error) {
	if err := idempotency.Check(key, false); err != nil {
		return Transaction{}, err
	}
	if in.Version < 1 {
		return Transaction{}, apierr.Validation(map[string]string{"version": "Send the version you loaded."})
	}
	var out Transaction
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		replay, err := idempotency.Begin(ctx, tx, userID, key, "transaction.update", idempotency.Hash("transaction.update", []any{id, in}))
		if err != nil {
			return err
		}
		if replay != nil {
			out, err = load(ctx, tx, userID, replay.ResourceID)
			return err
		}
		ls, err := legs(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		var cur Transaction
		for _, l := range ls {
			if l.ID == id {
				cur = l
			}
		}
		if cur.Version != in.Version {
			return ErrVersion
		}
		if cur.Status == "failed" || cur.Status == "cancelled" || cur.Status == "reversed" {
			return ErrClosed
		}
		if err := lockLegAccounts(ctx, tx, userID, ls); err != nil {
			return err
		}
		financial := in.financial(cur)
		if financial && (cur.Source == "group" || cur.Source == "settlement" || cur.Source == "topup") {
			return apierr.Validation(map[string]string{"amount": "This transaction belongs to a group. Void it from the group and record it again."})
		}
		if financial {
			var shared bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM group_expenses WHERE transaction_id=$1 AND voided_at IS NULL)`, cur.ID).Scan(&shared); err != nil {
				return err
			}
			if shared {
				return apierr.Validation(map[string]string{"amount": "This transaction is shared with a group or split. Remove the share first, then edit it."})
			}
		}
		if cur.Type == "transfer" && financial {
			return apierr.Validation(map[string]string{"amount": "A transfer's amount, accounts and date cannot be edited. Void it and record a new transfer."})
		}
		n := in.merge(cur)
		if in.Type != nil && n.Type != "expense" && n.Type != "income" && cur.Type != "transfer" {
			return apierr.Validation(map[string]string{"type": "Choose expense or income."})
		}
		if cur.Type != "transfer" && n.Type != cur.Type && cur.RefundOf != nil {
			return apierr.Validation(map[string]string{"type": "A refund must stay income."})
		}
		if f := n.validate(); len(f) > 0 {
			return apierr.Validation(f)
		}

		replace := financial && ledger.Counting(cur.Status)
		newID := id
		if replace {
			if cur.Type == "expense" {
				var refunds bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transactions WHERE refund_of=$1 AND status IN ('pending','confirmed'))`, cur.ID).Scan(&refunds); err != nil {
					return err
				}
				if refunds {
					return ErrHasRefunds
				}
			}
			// Reverse the original first so its source-event slot and refund share are free for the replacement.
			if err := applyStatus(ctx, tx, userID, cur, reversalTarget(cur.Status)); err != nil {
				return err
			}
			newID = ids.New()
			n.ID = newID
			n.ReplacesID = &cur.ID
			n.Source = cur.Source
			n.PostedAt = cur.PostedAt
			currency, err := prepare(ctx, tx, userID, &n, cur.ID, false)
			if err != nil {
				return err
			}
			if err := write(ctx, tx, userID, currency, n); err != nil {
				return err
			}
		} else {
			currency, err := prepare(ctx, tx, userID, &n, cur.ID, !financial)
			if err != nil {
				return err
			}
			// A needs-review edit may change account or amount: it has no ledger effect yet, so it is safe in place.
			tag, err := tx.Exec(ctx, `UPDATE transactions SET account_id=$3, type=$4, amount=$5, currency=$6, title=$7, description=$8,
				category_id=$9, payment_method=$10, payer_name=$11, payee_name=$12, occurred_at=$13, timezone=$14, notes=$15, location=$16,
				version=version+1, updated_at=now() WHERE id=$1 AND user_id=$2 AND version=$17`,
				id, userID, n.AccountID, n.Type, n.Amount, currency, n.Title, n.Description, n.CategoryID, n.PaymentMethod,
				n.PayerName, n.PayeeName, n.OccurredAt, n.Timezone, n.Notes, n.Location, in.Version)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrVersion
			}
			if in.Tags != nil {
				if err := setTags(ctx, tx, userID, id, n.Tags); err != nil {
					return err
				}
			}
		}
		if err := idempotency.Complete(ctx, tx, userID, key, "transaction", newID); err != nil {
			return err
		}
		out, err = load(ctx, tx, userID, newID)
		return err
	})
	return out, err
}

// reversalTarget is the final status a transaction takes when its financial fields are replaced.
func reversalTarget(status string) string {
	if status == "confirmed" {
		return "reversed"
	}
	return "cancelled"
}
