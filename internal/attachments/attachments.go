// Package attachments owns receipts and documents attached to transactions (CLAUDE.md 2.4, 5.5).
//
// Files go straight from the phone to private R2 objects through presigned URLs; the database holds metadata only.
// The server never trusts what the client says about a file: the signed URL fixes the exact size and content type,
// and on completion the server re-reads the object, checks its real size, sniffs its real type and (when given)
// verifies its checksum. A file that fails is deleted and marked rejected.
package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plinth/server/internal/platform/apierr"
	"plinth/server/internal/platform/dbx"
	"plinth/server/internal/platform/ids"
	"plinth/server/internal/platform/storage"
)

const (
	MaxSize        = 2 << 20 // 2 MB per file (CLAUDE.md 2.4)
	maxPerEntity   = 3       // proofs per transaction (product limit)
	uploadTTL      = 10 * time.Minute
	downloadTTL    = 5 * time.Minute
	abandonedAfter = time.Hour
)

var (
	ErrUnsupported = apierr.New(http.StatusUnprocessableEntity, "unsupported_file", "Attach a JPEG, PNG, WebP or PDF file.")
	ErrTooMany     = apierr.New(http.StatusConflict, "too_many_attachments", "A transaction can have up to 10 attachments.")
	ErrNotUploaded = apierr.New(http.StatusConflict, "upload_missing", "The file has not arrived yet. Upload it, then try again.")
	ErrRejected    = apierr.New(http.StatusUnprocessableEntity, "file_rejected", "That file did not pass the checks and was discarded.")
	ErrUnavailable = apierr.New(http.StatusServiceUnavailable, "storage_unavailable", "File storage is not set up on this server, so attachments are not available.")
	ErrNotReady    = apierr.New(http.StatusConflict, "not_uploaded", "That attachment is not available.")
)

// allowedExt maps each accepted type to the filename extensions that may accompany it. The extension is checked
// but never trusted on its own: the file's bytes decide.
var allowedExt = map[string][]string{
	"image/jpeg": {".jpg", ".jpeg"}, "image/png": {".png"}, "image/webp": {".webp"}, "application/pdf": {".pdf"},
}

type Attachment struct {
	ID         string     `json:"id"`
	EntityType string     `json:"entity_type"`
	EntityID   string     `json:"entity_id"`
	Filename   string     `json:"filename"`
	MimeType   string     `json:"mime_type"`
	Size       int64      `json:"size_bytes"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	UploadedAt *time.Time `json:"uploaded_at"`
}

type UploadTarget struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}

type Service struct {
	db    *pgxpool.Pool
	store storage.Store
	now   func() time.Time
}

func NewService(db *pgxpool.Pool, store storage.Store) *Service {
	return &Service{db: db, store: store, now: time.Now}
}

const cols = `id::text, entity_type, entity_id::text, original_filename, mime_type, size_bytes, status, created_at, uploaded_at`

func scan(row interface{ Scan(...any) error }) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.EntityType, &a.EntityID, &a.Filename, &a.MimeType, &a.Size, &a.Status, &a.CreatedAt, &a.UploadedAt)
	a.CreatedAt = a.CreatedAt.UTC()
	return a, err
}

// objectKey proves ownership by prefix and carries no personal text.
func objectKey(userID, attachmentID string) string { return "u/" + userID + "/a/" + attachmentID }

func ownsKey(userID, key string) bool { return strings.HasPrefix(key, "u/"+userID+"/a/") }

// Sniff returns the real type of a file from its first bytes, or "" if it is not an accepted type.
func Sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	case bytes.HasPrefix(b, []byte("%PDF-")):
		return "application/pdf"
	}
	return ""
}

func cleanFilename(s string) string {
	s = path.Base(strings.ReplaceAll(s, `\`, "/")) // drop any directory part first
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "." || s == ".." || s == "/" {
		return ""
	}
	if utf8.RuneCountInString(s) > 120 {
		r := []rune(s)
		s = string(r[len(r)-120:])
	}
	return s
}

type UploadInput struct {
	ID             string `json:"id"`
	TransactionID  string `json:"transaction_id"`
	Filename       string `json:"filename"`
	MimeType       string `json:"mime_type"`
	Size           int64  `json:"size_bytes"`
	ChecksumSHA256 string `json:"checksum_sha256"`
}

// RequestUpload validates what is about to be uploaded and returns a presigned URL for exactly that file.
func (s *Service) RequestUpload(ctx context.Context, userID string, in UploadInput) (Attachment, UploadTarget, error) {
	if s.store == nil {
		return Attachment{}, UploadTarget{}, ErrUnavailable
	}
	f := map[string]string{}
	exts, ok := allowedExt[in.MimeType]
	if !ok {
		return Attachment{}, UploadTarget{}, ErrUnsupported
	}
	name := cleanFilename(in.Filename)
	if name != "" {
		extOK := false
		for _, e := range exts {
			if strings.EqualFold(path.Ext(name), e) {
				extOK = true
			}
		}
		if !extOK {
			f["filename"] = "The file name does not match its type."
		}
	}
	if in.Size < 1 || in.Size > MaxSize {
		f["size_bytes"] = "A file can be at most 2 MB."
	}
	tid := ids.Normalize(in.TransactionID)
	if !ids.Valid(tid) {
		f["transaction_id"] = "Choose one of your transactions."
	}
	id := ids.Normalize(in.ID)
	if in.ID != "" && !ids.Valid(id) {
		f["id"] = "Identifier must be a UUID."
	}
	sum := strings.ToLower(in.ChecksumSHA256)
	if sum != "" && len(sum) != 64 {
		f["checksum_sha256"] = "Send a 64-character SHA-256 hex digest, or leave it out."
	}
	if len(f) > 0 {
		return Attachment{}, UploadTarget{}, apierr.Validation(f)
	}
	if id == "" {
		id = ids.New()
	}
	var out Attachment
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM transactions WHERE id=$1 AND user_id=$2`, tid, userID).Scan(&one); dbx.IsNoRows(err) {
			return apierr.Validation(map[string]string{"transaction_id": "Choose one of your transactions."})
		} else if err != nil {
			return err
		}
		// A retry with the same client id re-issues the URL for the same pending attachment.
		if prev, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM attachments WHERE id=$1 AND user_id=$2`, id, userID)); err == nil {
			if prev.Status == "pending" && prev.EntityID == tid && prev.MimeType == in.MimeType && prev.Size == in.Size {
				out = prev
				return nil
			}
			return apierr.New(http.StatusConflict, "id_conflict", "That identifier cannot be used. Generate a new one.")
		} else if !dbx.IsNoRows(err) {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM attachments WHERE entity_type='transaction' AND entity_id=$1 AND status IN ('pending','uploaded')`, tid).Scan(&n); err != nil {
			return err
		}
		if n >= maxPerEntity {
			return ErrTooMany
		}
		var err error
		out, err = scan(tx.QueryRow(ctx, `INSERT INTO attachments (id, user_id, entity_type, entity_id, object_key, original_filename, mime_type, size_bytes, checksum_sha256)
			VALUES ($1,$2,'transaction',$3,$4,$5,$6,$7,$8) RETURNING `+cols, id, userID, tid, objectKey(userID, id), name, in.MimeType, in.Size, sum))
		if err != nil {
			if dbx.PgCode(err) == dbx.UniqueViolation {
				return apierr.New(http.StatusConflict, "id_conflict", "That identifier cannot be used. Generate a new one.")
			}
			return err
		}
		return nil
	})
	if err != nil {
		return Attachment{}, UploadTarget{}, err
	}
	up, err := s.store.PresignPut(objectKey(userID, out.ID), out.MimeType, out.Size, uploadTTL)
	if err != nil {
		return Attachment{}, UploadTarget{}, err
	}
	return out, UploadTarget{URL: up.URL, Headers: up.Headers, ExpiresAt: up.Expires.UTC()}, nil
}

// Complete verifies the uploaded object. It is idempotent: completing an already-uploaded attachment returns it.
func (s *Service) Complete(ctx context.Context, userID, id string) (Attachment, error) {
	if s.store == nil {
		return Attachment{}, ErrUnavailable
	}
	var key, mime, sum string
	var size int64
	var status string
	err := s.db.QueryRow(ctx, `SELECT object_key, mime_type, size_bytes, checksum_sha256, status FROM attachments WHERE id=$1 AND user_id=$2`, id, userID).
		Scan(&key, &mime, &size, &sum, &status)
	if dbx.IsNoRows(err) {
		return Attachment{}, apierr.ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	switch status {
	case "uploaded":
		return s.get(ctx, userID, id)
	case "rejected", "deleted":
		return Attachment{}, ErrRejected
	}
	if !ownsKey(userID, key) { // defence in depth: the key always comes from the database, never the client
		return Attachment{}, apierr.ErrNotFound
	}
	reject := func(reason error) (Attachment, error) {
		_ = s.store.Delete(ctx, key)
		_, _ = s.db.Exec(ctx, `UPDATE attachments SET status='rejected', deleted_at=now(), object_removed=true WHERE id=$1 AND status='pending'`, id)
		return Attachment{}, reason
	}
	info, err := s.store.Head(ctx, key)
	if err == storage.ErrNotFound {
		return Attachment{}, ErrNotUploaded
	}
	if err != nil {
		return Attachment{}, err
	}
	if info.Size != size || info.Size > MaxSize {
		return reject(ErrRejected)
	}
	want := int64(512)
	if sum != "" {
		want = size // read it all (at most 2 MB) so the checksum can be verified
	}
	head, err := s.store.Range(ctx, key, want)
	if err != nil {
		return Attachment{}, err
	}
	if Sniff(head) != mime {
		return reject(ErrRejected)
	}
	if sum != "" {
		h := sha256.Sum256(head)
		if hex.EncodeToString(h[:]) != sum {
			return reject(ErrRejected)
		}
	}
	a, err := scan(s.db.QueryRow(ctx, `UPDATE attachments SET status='uploaded', uploaded_at=now() WHERE id=$1 AND user_id=$2 AND status='pending' RETURNING `+cols, id, userID))
	if dbx.IsNoRows(err) {
		return s.get(ctx, userID, id) // a concurrent completion won
	}
	if err != nil {
		return Attachment{}, err
	}
	return a, nil
}

func (s *Service) get(ctx context.Context, userID, id string) (Attachment, error) {
	a, err := scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM attachments WHERE id=$1 AND user_id=$2 AND status <> 'deleted'`, id, userID))
	if dbx.IsNoRows(err) {
		return a, apierr.ErrNotFound
	}
	return a, err
}

// DownloadURL returns a short-lived signed URL for an uploaded file the user owns.
func (s *Service) DownloadURL(ctx context.Context, userID, id string) (string, time.Time, error) {
	if s.store == nil {
		return "", time.Time{}, ErrUnavailable
	}
	var key, name, status string
	err := s.db.QueryRow(ctx, `SELECT object_key, original_filename, status FROM attachments WHERE id=$1 AND user_id=$2`, id, userID).Scan(&key, &name, &status)
	if dbx.IsNoRows(err) {
		return "", time.Time{}, apierr.ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if status != "uploaded" {
		return "", time.Time{}, ErrNotReady
	}
	if !ownsKey(userID, key) {
		return "", time.Time{}, apierr.ErrNotFound
	}
	return s.store.PresignGet(key, name, downloadTTL)
}

// Delete removes an attachment: the object is deleted and the row kept (marked deleted) as a record. If storage is
// unreachable the worker retries until the object is gone.
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	if s.store == nil {
		return ErrUnavailable
	}
	var key string
	err := s.db.QueryRow(ctx, `UPDATE attachments SET status='deleted', deleted_at=now() WHERE id=$1 AND user_id=$2 AND status IN ('pending','uploaded') RETURNING object_key`, id, userID).Scan(&key)
	if dbx.IsNoRows(err) {
		return apierr.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.store.Delete(ctx, key); err == nil {
		_, _ = s.db.Exec(ctx, `UPDATE attachments SET object_removed=true WHERE id=$1`, id)
	}
	return nil
}

func (s *Service) ForTransaction(ctx context.Context, userID, txID string) ([]Attachment, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM attachments WHERE entity_type='transaction' AND entity_id=$1 AND user_id=$2 AND status='uploaded' ORDER BY created_at, id`, txID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Cleanup removes abandoned uploads (pending for more than an hour) and retries object deletion for deleted or
// rejected attachments. Safe to run repeatedly and concurrently. It returns how many objects it removed.
func (s *Service) Cleanup(ctx context.Context, batch int) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	if _, err := s.db.Exec(ctx, `UPDATE attachments SET status='rejected', deleted_at=now() WHERE status='pending' AND created_at < $1`, s.now().Add(-abandonedAfter)); err != nil {
		return 0, err
	}
	rows, err := s.db.Query(ctx, `SELECT id::text, object_key FROM attachments WHERE status IN ('deleted','rejected') AND NOT object_removed ORDER BY created_at LIMIT $1`, batch)
	if err != nil {
		return 0, err
	}
	type item struct{ id, key string }
	var todo []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.key); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, it)
	}
	rows.Close()
	removed := 0
	for _, it := range todo {
		if err := s.store.Delete(ctx, it.key); err != nil {
			continue // try again next run
		}
		if _, err := s.db.Exec(ctx, `UPDATE attachments SET object_removed=true WHERE id=$1`, it.id); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
