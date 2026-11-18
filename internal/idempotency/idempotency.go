// Package idempotency makes financial writes safe to retry (CLAUDE.md 6.2). A request carries an
// Idempotency-Key header. The key is claimed inside the same database transaction as the write, so either both
// commit or neither does. A retry with the same key and the same request returns the original resource; the same
// key with a different request is rejected.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"

	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
)

var keyRe = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

var (
	ErrKeyRequired = apierr.New(http.StatusBadRequest, "idempotency_key_required",
		"This request needs an Idempotency-Key header (8 to 64 letters, digits, dot, dash or underscore).")
	ErrKeyInvalid = apierr.New(http.StatusBadRequest, "idempotency_key_invalid",
		"The Idempotency-Key header must be 8 to 64 letters, digits, dot, dash or underscore.")
	ErrKeyReused = apierr.New(http.StatusUnprocessableEntity, "idempotency_key_reused",
		"That Idempotency-Key was already used for a different request. Use a new key for a new request.")
)

// Check validates the key format. An empty key is allowed only when required is false.
func Check(key string, required bool) error {
	if key == "" {
		if required {
			return ErrKeyRequired
		}
		return nil
	}
	if !keyRe.MatchString(key) {
		return ErrKeyInvalid
	}
	return nil
}

// Hash fingerprints a request. Pass the operation scope and the validated, canonical request values.
func Hash(scope string, request any) []byte {
	b, _ := json.Marshal(request)
	h := sha256.Sum256(append([]byte(scope+"\x00"), b...))
	return h[:]
}

// Replay is the stored outcome of an earlier request with the same key.
type Replay struct {
	ResourceType string
	ResourceID   string
}

// Begin claims key for userID inside tx. It returns (nil, nil) when this is the first use and the caller must
// perform the write and then call Complete. It returns a Replay when the same request was already processed.
// A concurrent request with the same key waits on the unique index until the first one commits or aborts.
func Begin(ctx context.Context, tx pgx.Tx, userID, key, scope string, hash []byte) (*Replay, error) {
	if key == "" {
		return nil, nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, scope, request_hash) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		userID, key, scope, hash)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var gotScope string
	var gotHash []byte
	var r Replay
	var rid *string
	err = tx.QueryRow(ctx, `SELECT scope, request_hash, resource_type, resource_id::text FROM idempotency_keys WHERE user_id=$1 AND key=$2`,
		userID, key).Scan(&gotScope, &gotHash, &r.ResourceType, &rid)
	if err != nil {
		return nil, err
	}
	if gotScope != scope || string(gotHash) != string(hash) {
		return nil, ErrKeyReused
	}
	if rid == nil {
		return nil, apierr.New(http.StatusConflict, "request_in_progress", "That request is still being processed. Try again in a moment.")
	}
	r.ResourceID = *rid
	return &r, nil
}

// Complete records the resource created for key so a retry can replay it.
func Complete(ctx context.Context, q dbx.Q, userID, key, resourceType, resourceID string) error {
	if key == "" {
		return nil
	}
	_, err := q.Exec(ctx, `UPDATE idempotency_keys SET resource_type=$3, resource_id=$4 WHERE user_id=$1 AND key=$2`,
		userID, key, resourceType, resourceID)
	return err
}
