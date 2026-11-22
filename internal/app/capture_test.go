package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"plinth/server/internal/attachments"
)

// ---- attachments ----

var jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, bytes.Repeat([]byte{7}, 1000)...)
var pngBytes = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{1}, 500)...)

func key(uid, aid string) string { return "u/" + uid + "/a/" + aid }

func (e *env) requestUpload(t *testing.T, tok, tx string, body map[string]any) resp {
	t.Helper()
	b := map[string]any{"transaction_id": tx, "filename": "receipt.jpg", "mime_type": "image/jpeg", "size_bytes": len(jpegBytes)}
	for k, v := range body {
		b[k] = v
	}
	return e.call(t, "POST", "/v1/attachments/upload-url", tok, "", b)
}

func TestAttachmentLifecycle(t *testing.T) {
	e := setup(t)
	tok, _, uid := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1000)
	tx := str(e.spend(t, tok, a, 100, nil).data(), "id")

	r := e.requestUpload(t, tok, tx, nil)
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	att := r.data()["attachment"].(map[string]any)
	id := str(att, "id")
	up := r.data()["upload"].(map[string]any)
	if !strings.Contains(str(up, "url"), key(uid, id)) || str(att, "status") != "pending" {
		t.Fatalf("upload target: %s", r.Raw)
	}
	// The key carries no file name or personal text.
	if strings.Contains(str(up, "url"), "receipt") {
		t.Fatal("object key leaks the file name")
	}
	// Completing before the file arrives is a clear conflict, not success.
	if c := e.call(t, "POST", "/v1/attachments/complete", tok, "", map[string]any{"attachment_id": id}); c.Status != 409 || c.errCode() != "upload_missing" {
		t.Fatalf("complete before upload: %d %s", c.Status, c.Raw)
	}
	e.store.Put(key(uid, id), "image/jpeg", jpegBytes) // the phone's direct upload
	c := e.call(t, "POST", "/v1/attachments/complete", tok, "", map[string]any{"attachment_id": id})
	if c.Status != 200 || str(c.data(), "status") != "uploaded" {
		t.Fatalf("complete: %d %s", c.Status, c.Raw)
	}
	// Completing twice is harmless.
	if c2 := e.call(t, "POST", "/v1/attachments/complete", tok, "", map[string]any{"attachment_id": id}); c2.Status != 200 {
		t.Fatalf("second complete: %d", c2.Status)
	}
	if n := len(e.do(t, "GET", "/v1/transactions/"+tx+"/attachments", tok, nil).Body["data"].([]any)); n != 1 {
		t.Fatalf("listed %d", n)
	}
	d := e.do(t, "GET", "/v1/attachments/"+id+"/download-url", tok, nil)
	if d.Status != 200 || !strings.Contains(str(d.data(), "url"), key(uid, id)) || d.data()["expires_at"] == nil {
		t.Fatalf("download url: %d %s", d.Status, d.Raw)
	}
	if del := e.call(t, "DELETE", "/v1/attachments/"+id, tok, "", nil); del.Status != 204 {
		t.Fatalf("delete: %d", del.Status)
	}
	if e.store.Has(key(uid, id)) {
		t.Fatal("deleting an attachment must remove the object")
	}
	if n := len(e.do(t, "GET", "/v1/transactions/"+tx+"/attachments", tok, nil).Body["data"].([]any)); n != 0 {
		t.Fatal("deleted attachment still listed")
	}
	if d2 := e.do(t, "GET", "/v1/attachments/"+id+"/download-url", tok, nil); d2.Status != 409 {
		t.Fatalf("download after delete: %d", d2.Status)
	}
	// The transaction itself is untouched by attachment changes.
	if e.balance(t, tok, a) != 900 {
		t.Fatal("attachments must not change a balance")
	}
}

func TestAttachmentUploadValidation(t *testing.T) { // exit criterion: upload validation tests pass
	e := setup(t)
	tok, _, uid := e.signUp(t, "asha@example.com")
	other, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1000)
	tx := str(e.spend(t, tok, a, 100, nil).data(), "id")
	oa := e.newAccount(t, other, "B", "savings", 1000)
	otx := str(e.spend(t, other, oa, 100, nil).data(), "id")

	for name, body := range map[string]map[string]any{
		"too big":            {"size_bytes": 2*1024*1024 + 1},
		"empty":              {"size_bytes": 0},
		"html":               {"mime_type": "text/html", "filename": "x.html"},
		"svg":                {"mime_type": "image/svg+xml", "filename": "x.svg"},
		"exe as jpeg name":   {"filename": "malware.exe"},
		"png name, jpeg":     {"filename": "receipt.png"},
		"bad transaction id": {"transaction_id": "nope"},
		"foreign tx":         {"transaction_id": otx},
		"bad checksum":       {"checksum_sha256": "abc"},
		"bad id":             {"id": "nope"},
	} {
		if r := e.requestUpload(t, tok, tx, body); r.Status != 422 && r.Status != 400 {
			t.Errorf("%s: want 4xx got %d %s", name, r.Status, r.Raw)
		}
	}
	// Exactly 2 MB is allowed; a path in the file name is reduced to the name.
	if r := e.requestUpload(t, tok, tx, map[string]any{"size_bytes": 2 * 1024 * 1024, "filename": "../../etc/passwd.jpg"}); r.Status != 201 || str(r.data()["attachment"].(map[string]any), "filename") != "passwd.jpg" {
		t.Fatalf("2 MB / path name: %d %s", r.Status, r.Raw)
	}
	// At most 3 proofs per transaction (one pending already exists above).
	for i := 0; i < 2; i++ {
		if r := e.requestUpload(t, tok, tx, nil); r.Status != 201 {
			t.Fatalf("attachment %d: %d %s", i+2, r.Status, r.Raw)
		}
	}
	if r := e.requestUpload(t, tok, tx, nil); r.Status != 409 || r.errCode() != "too_many_attachments" {
		t.Fatalf("4th: %d %s", r.Status, r.Raw)
	}
	_ = uid
}

func TestAttachmentContentIsVerifiedNotTrusted(t *testing.T) {
	e := setup(t)
	tok, _, uid := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1000)
	tx := str(e.spend(t, tok, a, 100, nil).data(), "id")
	upload := func(body map[string]any, data []byte, ctype string) (string, resp) {
		r := e.requestUpload(t, tok, tx, body)
		if r.Status != 201 {
			t.Fatalf("%d %s", r.Status, r.Raw)
		}
		id := str(r.data()["attachment"].(map[string]any), "id")
		e.store.Put(key(uid, id), ctype, data)
		return id, e.call(t, "POST", "/v1/attachments/complete", tok, "", map[string]any{"attachment_id": id})
	}
	// An HTML or script file renamed and labelled as a JPEG is rejected and its object deleted.
	html := []byte("<html><script>alert(1)</script></html>" + strings.Repeat("x", 962))
	id, c := upload(map[string]any{"size_bytes": len(html)}, html, "image/jpeg")
	if c.Status != 422 || c.errCode() != "file_rejected" || e.store.Has(key(uid, id)) {
		t.Fatalf("disguised file: %d %s (object kept: %v)", c.Status, c.Raw, e.store.Has(key(uid, id)))
	}
	// A PNG declared as JPEG is rejected: the declared type must match the bytes.
	id, c = upload(map[string]any{"size_bytes": len(pngBytes)}, pngBytes, "image/jpeg")
	if c.Status != 422 || e.store.Has(key(uid, id)) {
		t.Fatalf("png as jpeg: %d", c.Status)
	}
	// The same bytes declared honestly as PNG pass.
	if _, c = upload(map[string]any{"size_bytes": len(pngBytes), "mime_type": "image/png", "filename": "r.png"}, pngBytes, "image/png"); c.Status != 200 {
		t.Fatalf("honest png: %d %s", c.Status, c.Raw)
	}
	// The object's real size must equal what was declared and signed.
	id, c = upload(map[string]any{"size_bytes": len(jpegBytes)}, append(jpegBytes, bytes.Repeat([]byte{1}, 50)...), "image/jpeg")
	if c.Status != 422 || e.store.Has(key(uid, id)) {
		t.Fatalf("oversized upload: %d", c.Status)
	}
	// Checksum: right passes, wrong is rejected.
	sum := sha256.Sum256(jpegBytes)
	if _, c = upload(map[string]any{"checksum_sha256": hex.EncodeToString(sum[:])}, jpegBytes, "image/jpeg"); c.Status != 200 {
		t.Fatalf("right checksum: %d %s", c.Status, c.Raw)
	}
	bad := sha256.Sum256([]byte("other"))
	if id, c = upload(map[string]any{"checksum_sha256": hex.EncodeToString(bad[:])}, jpegBytes, "image/jpeg"); c.Status != 422 || e.store.Has(key(uid, id)) {
		t.Fatalf("wrong checksum: %d", c.Status)
	}
	// A rejected attachment can never be completed or downloaded.
	if again := e.call(t, "POST", "/v1/attachments/complete", tok, "", map[string]any{"attachment_id": id}); again.Status != 422 {
		t.Fatalf("complete after reject: %d", again.Status)
	}
	if d := e.do(t, "GET", "/v1/attachments/"+id+"/download-url", tok, nil); d.Status != 409 {
		t.Fatalf("download rejected: %d", d.Status)
	}
}

func TestAttachmentOwnership(t *testing.T) {
	e := setup(t)
	ta, _, uidA := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, ta, "A", "savings", 1000)
	tx := str(e.spend(t, ta, a, 100, nil).data(), "id")
	r := e.requestUpload(t, ta, tx, nil)
	id := str(r.data()["attachment"].(map[string]any), "id")
	e.store.Put(key(uidA, id), "image/jpeg", jpegBytes)
	if c := e.call(t, "POST", "/v1/attachments/complete", ta, "", map[string]any{"attachment_id": id}); c.Status != 200 {
		t.Fatal(c.Raw)
	}
	for _, c := range []struct{ m, p string }{
		{"GET", "/v1/attachments/" + id + "/download-url"}, {"DELETE", "/v1/attachments/" + id}, {"GET", "/v1/transactions/" + tx + "/attachments"},
	} {
		got := e.call(t, c.m, c.p, tb, "", nil)
		if c.m == "GET" && strings.HasSuffix(c.p, "/attachments") {
			if got.Status != 200 || strings.Contains(got.Raw, id) {
				t.Fatalf("listing leaked: %d %s", got.Status, got.Raw)
			}
			continue
		}
		if got.Status != 404 {
			t.Fatalf("%s %s by another user: %d", c.m, c.p, got.Status)
		}
	}
	if c := e.call(t, "POST", "/v1/attachments/complete", tb, "", map[string]any{"attachment_id": id}); c.Status != 404 {
		t.Fatalf("complete by another user: %d", c.Status)
	}
	if !e.store.Has(key(uidA, id)) {
		t.Fatal("another user's request deleted the object")
	}
	if g := e.do(t, "GET", "/v1/attachments/"+id+"/download-url", "", nil); g.Status != 401 {
		t.Fatalf("unauthenticated: %d", g.Status)
	}
}

func TestAttachmentCleanupRemovesAbandonedUploads(t *testing.T) {
	e := setup(t)
	tok, _, uid := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1000)
	tx := str(e.spend(t, tok, a, 100, nil).data(), "id")
	r := e.requestUpload(t, tok, tx, nil)
	id := str(r.data()["attachment"].(map[string]any), "id")
	e.store.Put(key(uid, id), "image/jpeg", jpegBytes) // uploaded but never completed
	if _, err := pool.Exec(context.Background(), `UPDATE attachments SET created_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	svc := attachments.NewService(pool, e.store)
	n, err := svc.Cleanup(context.Background(), 50)
	if err != nil || n != 1 || e.store.Has(key(uid, id)) {
		t.Fatalf("cleanup removed %d (err %v), object kept: %v", n, err, e.store.Has(key(uid, id)))
	}
	var status string
	_ = pool.QueryRow(context.Background(), `SELECT status FROM attachments WHERE id=$1`, id).Scan(&status)
	if status != "rejected" {
		t.Fatalf("abandoned upload should be rejected, got %s", status)
	}
	// Running it again finds nothing (safe to retry).
	if n, _ := svc.Cleanup(context.Background(), 50); n != 0 {
		t.Fatalf("second run removed %d", n)
	}
	// A recent pending upload is left alone.
	r2 := e.requestUpload(t, tok, tx, nil)
	id2 := str(r2.data()["attachment"].(map[string]any), "id")
	e.store.Put(key(uid, id2), "image/jpeg", jpegBytes)
	if n, _ := svc.Cleanup(context.Background(), 50); n != 0 || !e.store.Has(key(uid, id2)) {
		t.Fatal("cleanup removed a recent upload")
	}
}

func TestAttachmentsAreUnavailableWithoutStorage(t *testing.T) {
	e := setupWith(t, false)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "A", "savings", 1000)
	tx := str(e.spend(t, tok, a, 100, nil).data(), "id")
	if r := e.requestUpload(t, tok, tx, nil); r.Status != 503 || r.errCode() != "storage_unavailable" {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
}

// ---- CSV import and export ----

const sampleCSV = "Date,Description,Amount,Reference,Category\n" +
	"2026-11-02,Swiggy dinner,-482.00,UPI100001,Food and dining\n" +
	"2026-11-03,Salary,\"1,12,400.00\",NEFT55555,Salary\n" +
	"2026-11-04,Reliance Fresh,-1250.50,,Groceries\n" +
	"2026-13-40,Bad date row,-10,,\n" +
	"2026-11-05,,-20,,\n" +
	"2026-11-06,Not a number,abc,,\n"

func (e *env) upload(t *testing.T, tok, acct, content string) resp {
	t.Helper()
	return e.call(t, "POST", "/v1/imports", tok, "", map[string]any{"account_id": acct, "filename": "statement.csv", "content": content})
}

func stdMapping() map[string]any {
	return map[string]any{"date": "Date", "title": "Description", "amount": "Amount", "reference": "Reference", "category": "Category", "date_format": "ymd"}
}

func TestCSVImportEndToEnd(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	up := e.upload(t, tok, a, sampleCSV)
	if up.Status != 201 {
		t.Fatalf("upload: %d %s", up.Status, up.Raw)
	}
	id := str(up.data(), "id")
	if hs := up.data()["headers"].([]any); len(hs) != 5 || up.data()["suggested_mapping"] == nil {
		t.Fatalf("headers/suggested mapping: %s", up.Raw)
	}
	if str(up.data()["suggested_mapping"].(map[string]any), "amount") != "Amount" {
		t.Fatal("the mapping should be guessed from the headers")
	}
	// Nothing is created by uploading.
	if e.balance(t, tok, a) != 1000000 {
		t.Fatal("upload changed a balance")
	}
	m := e.call(t, "PUT", "/v1/imports/"+id+"/mapping", tok, "", stdMapping())
	if m.Status != 200 {
		t.Fatalf("mapping: %d %s", m.Status, m.Raw)
	}
	d := m.data()
	if i64(d, "total_rows") != 6 || i64(d, "valid_rows") != 3 || i64(d, "invalid_rows") != 3 || i64(d, "duplicate_rows") != 0 {
		t.Fatalf("counts: %v", d)
	}
	if i64(d, "total_debit") != 48200+125050 || i64(d, "total_credit") != 11240000 {
		t.Fatalf("totals: debit %d credit %d", i64(d, "total_debit"), i64(d, "total_credit"))
	}
	if e.balance(t, tok, a) != 1000000 {
		t.Fatal("mapping changed a balance")
	}
	errs := e.do(t, "GET", "/v1/imports/"+id+"/errors", tok, nil).data()["rows"].([]any)
	if len(errs) != 3 {
		t.Fatalf("errors: %v", errs)
	}
	first := errs[0].(map[string]any)
	if i64(first, "row") != 5 || !strings.Contains(str(first, "error"), "Date is not valid") { // spreadsheet row numbers: header is row 1
		t.Fatalf("first error: %v", first)
	}
	// Invalid rows are never imported.
	c := e.call(t, "POST", "/v1/imports/"+id+"/confirm", tok, "", map[string]any{})
	if c.Status != 200 || i64(c.data(), "created_count") != 3 || str(c.data(), "status") != "confirmed" {
		t.Fatalf("confirm: %d %s", c.Status, c.Raw)
	}
	want := int64(1000000 - 48200 - 125050 + 11240000)
	if e.balance(t, tok, a) != want {
		t.Fatalf("balance %d want %d", e.balance(t, tok, a), want)
	}
	txs := e.do(t, "GET", "/v1/transactions?limit=50", tok, nil).Body["data"].([]any)
	if len(txs) != 3 {
		t.Fatalf("transactions: %d", len(txs))
	}
	var sawCat bool
	for _, x := range txs {
		m := x.(map[string]any)
		if str(m, "source") != "csv" || str(m, "status") != "confirmed" {
			t.Fatalf("imported transaction: %v", m)
		}
		if str(m, "title") == "Swiggy dinner" && m["category_id"] != nil {
			sawCat = true
		}
	}
	if !sawCat {
		t.Fatal("category names should map to the user's categories")
	}
	// Confirming again returns the same result and creates nothing.
	if c2 := e.call(t, "POST", "/v1/imports/"+id+"/confirm", tok, "", map[string]any{}); c2.Status != 200 || i64(c2.data(), "created_count") != 3 || e.balance(t, tok, a) != want {
		t.Fatalf("second confirm: %d %s", c2.Status, c2.Raw)
	}
	// Re-uploading the very same file as a new batch cannot duplicate anything.
	up2 := e.upload(t, tok, a, sampleCSV)
	id2 := str(up2.data(), "id")
	m2 := e.call(t, "PUT", "/v1/imports/"+id2+"/mapping", tok, "", stdMapping())
	if i64(m2.data(), "duplicate_rows") != 3 || i64(m2.data(), "valid_rows") != 0 {
		t.Fatalf("re-import should be all duplicates: %v", m2.data())
	}
	c3 := e.call(t, "POST", "/v1/imports/"+id2+"/confirm", tok, "", map[string]any{})
	if i64(c3.data(), "created_count") != 0 || e.balance(t, tok, a) != want {
		t.Fatalf("a retried import created records: %s", c3.Raw)
	}
	// Undo voids everything the first batch created; history is kept.
	u := e.call(t, "POST", "/v1/imports/"+id+"/undo", tok, "", nil)
	if u.Status != 200 || str(u.data(), "status") != "undone" {
		t.Fatalf("undo: %d %s", u.Status, u.Raw)
	}
	if e.balance(t, tok, a) != 1000000 {
		t.Fatalf("balance after undo %d", e.balance(t, tok, a))
	}
	if n := len(e.do(t, "GET", "/v1/transactions?status=reversed&limit=50", tok, nil).Body["data"].([]any)); n != 3 {
		t.Fatalf("undone transactions should stay as reversed history, got %d", n)
	}
	if u2 := e.call(t, "POST", "/v1/imports/"+id+"/undo", tok, "", nil); u2.Status != 200 || e.balance(t, tok, a) != 1000000 {
		t.Fatalf("second undo: %d", u2.Status)
	}
	// After the undo the same file can be imported again.
	up3 := e.upload(t, tok, a, sampleCSV)
	id3 := str(up3.data(), "id")
	m3 := e.call(t, "PUT", "/v1/imports/"+id3+"/mapping", tok, "", stdMapping())
	if i64(m3.data(), "valid_rows") != 3 {
		t.Fatalf("after undo rows are importable again: %v", m3.data())
	}
	assertLedger(t)
}

func TestCSVFormats(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 0)
	// Separate debit and credit columns, dd/mm/yyyy dates, Indian grouping, semicolon delimiter, BOM.
	csv := "\ufeffTxn Date;Narration;Withdrawal Amt.;Deposit Amt.;Chq./Ref.No.\n" +
		"18/11/2026;UPI-SWIGGY;1,482.50;;REF0001\n" +
		"17/11/2026;SALARY CREDIT;;1,12,400.00;REF0002\n" +
		"16/11/2026;ATM;;;REF0003\n" +
		"15/11/2026;BOTH;10.00;5.00;REF0004\n"
	id := str(e.upload(t, tok, a, csv).data(), "id")
	m := e.call(t, "PUT", "/v1/imports/"+id+"/mapping", tok, "", map[string]any{"date": "Txn Date", "title": "Narration", "debit": "Withdrawal Amt.", "credit": "Deposit Amt.", "reference": "Chq./Ref.No.", "date_format": "dmy"})
	if m.Status != 200 || i64(m.data(), "valid_rows") != 2 || i64(m.data(), "invalid_rows") != 2 || i64(m.data(), "total_debit") != 148250 || i64(m.data(), "total_credit") != 11240000 {
		t.Fatalf("%d %s", m.Status, m.Raw)
	}
	// Dr/Cr suffixes and a flipped sign convention.
	csv2 := "Date,Details,Amt\n2026-11-01,Rent,\"28,000.00 Dr\"\n2026-11-02,Refund,\"500.00 Cr\"\n"
	id2 := str(e.upload(t, tok, a, csv2).data(), "id")
	m2 := e.call(t, "PUT", "/v1/imports/"+id2+"/mapping", tok, "", map[string]any{"date": "Date", "title": "Details", "amount": "Amt", "date_format": "ymd"})
	if i64(m2.data(), "total_debit") != 2800000 || i64(m2.data(), "total_credit") != 50000 {
		t.Fatalf("Dr/Cr: %s", m2.Raw)
	}
	csv3 := "Date,Desc,Amount\n2026-11-01,Card payment,250.00\n2026-11-02,Payment received,-100.00\n"
	id3 := str(e.upload(t, tok, a, csv3).data(), "id")
	m3 := e.call(t, "PUT", "/v1/imports/"+id3+"/mapping", tok, "", map[string]any{"date": "Date", "title": "Desc", "amount": "Amount", "date_format": "ymd", "positive_is_debit": true})
	if i64(m3.data(), "total_debit") != 25000 || i64(m3.data(), "total_credit") != 10000 {
		t.Fatalf("positive_is_debit: %s", m3.Raw)
	}
}

func TestCSVDuplicateCandidatesAndRetriesAreSafe(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	// A transaction the user already entered by hand.
	e.spend(t, tok, a, 48200, map[string]any{"title": "Swiggy dinner", "occurred_at": "2026-11-02T12:00:00+05:30"})
	csv := "Date,Description,Amount,Reference\n2026-11-02,SWIGGY DINNER,-482.00,\n2026-11-03,Chai,-20.00,\n2026-11-03,Chai,-20.00,\n"
	id := str(e.upload(t, tok, a, csv).data(), "id")
	m := e.call(t, "PUT", "/v1/imports/"+id+"/mapping", tok, "", map[string]any{"date": "Date", "title": "Description", "amount": "Amount", "reference": "Reference", "date_format": "ymd"})
	if i64(m.data(), "duplicate_rows") != 1 || i64(m.data(), "valid_rows") != 2 {
		t.Fatalf("one probable duplicate, two genuine identical rows: %s", m.Raw)
	}
	prev := e.do(t, "GET", "/v1/imports/"+id+"/preview?status=duplicate", tok, nil).data()["rows"].([]any)
	if len(prev) != 1 || !strings.Contains(str(prev[0].(map[string]any), "error"), "already have") {
		t.Fatalf("duplicate preview: %v", prev)
	}
	// Two identical rows in one file are both imported (they are two coffees), once each.
	c := e.call(t, "POST", "/v1/imports/"+id+"/confirm", tok, "", map[string]any{})
	if i64(c.data(), "created_count") != 2 || e.balance(t, tok, a) != 1000000-48200-4000 {
		t.Fatalf("confirm: %s balance %d", c.Raw, e.balance(t, tok, a))
	}
	// Including the duplicate row explicitly is allowed on a fresh batch.
	id2 := str(e.upload(t, tok, a, "Date,Description,Amount\n2026-11-02,Swiggy dinner,-482.00\n").data(), "id")
	e.call(t, "PUT", "/v1/imports/"+id2+"/mapping", tok, "", map[string]any{"date": "Date", "title": "Description", "amount": "Amount", "date_format": "ymd"})
	c2 := e.call(t, "POST", "/v1/imports/"+id2+"/confirm", tok, "", map[string]any{"include_duplicate_rows": []int{2}})
	if i64(c2.data(), "created_count") != 1 {
		t.Fatalf("explicitly included duplicate: %s", c2.Raw)
	}
	assertLedger(t)
}

func TestCSVConcurrentConfirmCreatesOnce(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 1000000)
	id := str(e.upload(t, tok, a, sampleCSV).data(), "id")
	e.call(t, "PUT", "/v1/imports/"+id+"/mapping", tok, "", stdMapping())
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e.call(t, "POST", "/v1/imports/"+id+"/confirm", tok, "", map[string]any{}); r.Status != 200 {
				t.Errorf("confirm: %d %s", r.Status, r.Raw)
			}
		}()
	}
	wg.Wait()
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM transactions WHERE source='csv'`).Scan(&n)
	if n != 3 {
		t.Fatalf("concurrent confirms created %d transactions", n)
	}
	assertLedger(t)
}

func TestCSVValidationAndOwnership(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, ta, "A", "savings", 0)
	b := e.newAccount(t, tb, "B", "savings", 0)
	for name, r := range map[string]resp{
		"empty":           e.upload(t, ta, a, ""),
		"header only":     e.upload(t, ta, a, "Date,Amount\n"),
		"foreign account": e.upload(t, ta, b, sampleCSV),
		"bad account":     e.upload(t, ta, "nope", sampleCSV),
		"too many rows":   e.upload(t, ta, a, "Date,Description,Amount\n"+strings.Repeat("2026-01-01,x,-1\n", 5001)),
	} {
		if r.Status != 422 && r.Status != 400 {
			t.Errorf("%s: want 4xx got %d", name, r.Status)
		}
	}
	id := str(e.upload(t, ta, a, sampleCSV).data(), "id")
	if c := e.call(t, "POST", "/v1/imports/"+id+"/confirm", ta, "", map[string]any{}); c.Status != 409 {
		t.Fatalf("confirming an unmapped import: %d", c.Status)
	}
	for name, mp := range map[string]map[string]any{
		"unknown column": {"date": "Nope", "title": "Description", "amount": "Amount"},
		"no amount":      {"date": "Date", "title": "Description"},
		"no date":        {"title": "Description", "amount": "Amount"},
		"bad format":     {"date": "Date", "title": "Description", "amount": "Amount", "date_format": "xyz"},
	} {
		if r := e.call(t, "PUT", "/v1/imports/"+id+"/mapping", ta, "", mp); r.Status != 422 {
			t.Errorf("%s: want 422 got %d %s", name, r.Status, r.Raw)
		}
	}
	for _, c := range []struct{ m, p string }{{"GET", "/v1/imports/" + id}, {"GET", "/v1/imports/" + id + "/preview"}, {"PUT", "/v1/imports/" + id + "/mapping"}, {"POST", "/v1/imports/" + id + "/confirm"}, {"POST", "/v1/imports/" + id + "/undo"}} {
		body := stdMapping()
		if strings.HasSuffix(c.p, "confirm") {
			body = map[string]any{}
		}
		if r := e.call(t, c.m, c.p, tb, "", body); r.Status != 404 {
			t.Errorf("%s %s by another user: %d", c.m, c.p, r.Status)
		}
	}
	if strings.Contains(e.do(t, "GET", "/v1/imports", tb, nil).Raw, id) {
		t.Fatal("import list leaked")
	}
	tpl := e.call(t, "GET", "/v1/imports/template", ta, "", nil)
	if tpl.Status != 200 || !strings.HasPrefix(tpl.Raw, "Date,Description,Amount") {
		t.Fatalf("template: %d %s", tpl.Status, tpl.Raw)
	}
}

func TestCSVExportIsProtectedAndSafe(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, ta, "HDFC", "savings", 100000)
	b := e.newAccount(t, tb, "Ravi secret", "savings", 100000)
	e.spend(t, ta, a, 50000, map[string]any{"title": "=HYPERLINK(\"http://evil\",\"x\")", "occurred_at": "2026-11-10T10:00:00Z", "notes": "+cmd"})
	e.spend(t, ta, a, 1234, map[string]any{"title": "Coffee, \"large\"", "occurred_at": "2026-10-10T10:00:00Z"})
	e.spend(t, tb, b, 999, map[string]any{"title": "Ravi private"})

	req := func(tok string, body any) (*http.Response, string) {
		var rd io.Reader
		if body != nil {
			rd = strings.NewReader(fmt.Sprint(jsonOf(body)))
		}
		rq, _ := http.NewRequest("POST", e.srv.URL+"/v1/exports", rd)
		if tok != "" {
			rq.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := http.DefaultClient.Do(rq)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	res, body := req(ta, map[string]any{})
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/csv") || res.Header.Get("Cache-Control") != "no-store" || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("headers: %d %v", res.StatusCode, res.Header)
	}
	if strings.Contains(body, "Ravi") || strings.Contains(body, "private") {
		t.Fatal("an export leaked another user's data")
	}
	if !strings.Contains(body, "'=HYPERLINK") || !strings.Contains(body, "'+cmd") {
		t.Fatalf("formula cells must be neutralised:\n%s", body)
	}
	if !strings.Contains(body, "-500.00") || !strings.Contains(body, `"Coffee, ""large"""`) {
		t.Fatalf("amounts are signed decimals and quoting is correct:\n%s", body)
	}
	_, ranged := req(ta, map[string]any{"from": "2026-11-01", "to": "2026-11-30"})
	if strings.Contains(ranged, "Coffee") || !strings.Contains(ranged, "HYPERLINK") {
		t.Fatalf("date range:\n%s", ranged)
	}
	if res, _ := req(ta, map[string]any{"account_id": b}); res.StatusCode != 404 {
		t.Fatalf("exporting another user's account: %d", res.StatusCode)
	}
	if res, _ := req("", nil); res.StatusCode != 401 {
		t.Fatalf("unauthenticated export: %d", res.StatusCode)
	}
}

func countTx(t *testing.T) int {
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM transactions`).Scan(&n)
	return n
}

// ---- bank SMS ----

func fp(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (e *env) sms(t *testing.T, tok string, body map[string]any) resp {
	t.Helper()
	b := map[string]any{"fingerprint": fp(fmt.Sprint(body)), "sender": "HDFCBK", "received_at": time.Now().UTC().Format(time.RFC3339), "amount": 48200, "direction": "debit",
		"account_hint": "4821", "merchant": "Swiggy", "reference": "UPI4821991", "confidence": 90}
	for k, v := range body {
		b[k] = v
	}
	return e.call(t, "POST", "/v1/sms-events", tok, "", b)
}

func TestSMSDuplicateMessageCreatesNoSecondTransaction(t *testing.T) { // exit criterion
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := str(e.call(t, "POST", "/v1/accounts", tok, newKey(), map[string]any{"name": "HDFC", "type": "savings", "opening_balance": 100000, "masked_identifier": "4821"}).data(), "id")
	f := fp("HDFC Bank: Rs 482.00 debited a/c XX4821 to SWIGGY on 18-11-26. UPI Ref 4821991")
	first := e.sms(t, tok, map[string]any{"fingerprint": f})
	if first.Status != 200 || str(first.data(), "status") != "review" {
		t.Fatalf("ingest: %d %s", first.Status, first.Raw)
	}
	// The account is suggested (one account ends in 4821) but never assigned.
	if str(first.data(), "suggested_account_id") != a || first.data()["account_id"] != nil {
		t.Fatalf("suggestion only: %s", first.Raw)
	}
	if countTx(t) != 0 {
		t.Fatal("ingesting must not create a transaction")
	}
	// The same message processed again (or synced from another device) is the same event.
	for i := 0; i < 3; i++ {
		again := e.sms(t, tok, map[string]any{"fingerprint": f})
		if str(again.data(), "id") != str(first.data(), "id") {
			t.Fatal("same fingerprint must return the same event")
		}
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM sms_events`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d events for one message", n)
	}
	id := str(first.data(), "id")
	k := newKey()
	add := e.call(t, "POST", "/v1/sms-events/"+id+"/add", tok, k, map[string]any{"account_id": a})
	if add.Status != 200 || str(add.data(), "status") != "added" || add.data()["transaction_id"] == nil {
		t.Fatalf("add: %d %s", add.Status, add.Raw)
	}
	tx := e.do(t, "GET", "/v1/transactions/"+str(add.data(), "transaction_id"), tok, nil).data()
	if str(tx, "source") != "sms" || i64(tx, "amount") != 48200 || str(tx, "title") != "Swiggy" {
		t.Fatalf("tx: %v", tx)
	}
	if e.balance(t, tok, a) != 100000-48200 {
		t.Fatal("balance")
	}
	// Retrying the same add replays; a different key finds it already handled. No second transaction either way.
	if r := e.call(t, "POST", "/v1/sms-events/"+id+"/add", tok, k, map[string]any{"account_id": a}); r.Status != 200 {
		t.Fatalf("replay: %d", r.Status)
	}
	if r := e.call(t, "POST", "/v1/sms-events/"+id+"/add", tok, newKey(), map[string]any{"account_id": a}); r.Status != 409 {
		t.Fatalf("second add: %d", r.Status)
	}
	// The bank re-sends the same event in a different message (new text, same reference): recognised as a duplicate.
	dup := e.sms(t, tok, map[string]any{"fingerprint": fp("a different text for the same debit")})
	if str(dup.data(), "status") != "duplicate" || dup.data()["duplicate_of"] == nil {
		t.Fatalf("same bank reference must be flagged: %s", dup.Raw)
	}
	if countTx(t) != 1 || e.balance(t, tok, a) != 100000-48200 {
		t.Fatal("a duplicate message created money movement")
	}
	assertLedger(t)
}

func TestSMSNeverAutoAssignsAnAccount(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	mk := func(name, mask string) string {
		return str(e.call(t, "POST", "/v1/accounts", tok, newKey(), map[string]any{"name": name, "type": "savings", "masked_identifier": mask}).data(), "id")
	}
	one, two := mk("HDFC", "4821"), mk("ICICI card", "4821") // two accounts share the same last four
	_ = one
	_ = two
	ambiguous := e.sms(t, tok, map[string]any{"reference": "R1000001"})
	if ambiguous.data()["suggested_account_id"] != nil || ambiguous.data()["account_id"] != nil || str(ambiguous.data(), "status") != "review" {
		t.Fatalf("ambiguous evidence must not suggest anything: %s", ambiguous.Raw)
	}
	none := e.sms(t, tok, map[string]any{"account_hint": "9999", "reference": "R1000002"})
	if none.data()["suggested_account_id"] != nil {
		t.Fatalf("unknown account: %s", none.Raw)
	}
	noHint := e.sms(t, tok, map[string]any{"account_hint": "", "reference": "R1000003"})
	if noHint.data()["suggested_account_id"] != nil {
		t.Fatalf("no hint: %s", noHint.Raw)
	}
	// Adding needs an explicit account, in the right currency.
	id := str(noHint.data(), "id")
	if r := e.call(t, "POST", "/v1/sms-events/"+id+"/add", tok, newKey(), map[string]any{}); r.Status != 422 {
		t.Fatalf("add without an account: %d", r.Status)
	}
	usd := str(e.call(t, "POST", "/v1/accounts", tok, newKey(), map[string]any{"name": "USD", "type": "savings", "currency": "USD"}).data(), "id")
	if r := e.call(t, "POST", "/v1/sms-events/"+id+"/add", tok, newKey(), map[string]any{"account_id": usd}); r.Status != 422 {
		t.Fatalf("wrong currency: %d", r.Status)
	}
	if countTx(t) != 0 {
		t.Fatal("nothing may be recorded without review")
	}
}

func TestSMSMatchesExistingTransactions(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := str(e.call(t, "POST", "/v1/accounts", tok, newKey(), map[string]any{"name": "HDFC", "type": "savings", "opening_balance": 1000000, "masked_identifier": "4821"}).data(), "id")
	// Entered by hand first; the bank message arrives later.
	e.spend(t, tok, a, 12000, map[string]any{"title": "Uber to airport"})
	dup := e.sms(t, tok, map[string]any{"amount": 12000, "merchant": "UBER INDIA", "reference": ""})
	if str(dup.data(), "status") != "duplicate" {
		t.Fatalf("a message for a transaction the user already added: %s", dup.Raw)
	}
	// The user can still add it anyway.
	if r := e.call(t, "POST", "/v1/sms-events/"+str(dup.data(), "id")+"/add", tok, newKey(), map[string]any{"account_id": a}); r.Status != 200 {
		t.Fatalf("add anyway: %d %s", r.Status, r.Raw)
	}
	// A different amount on the same day is a different transaction.
	other := e.sms(t, tok, map[string]any{"amount": 99900, "merchant": "Zomato", "reference": "R7"})
	if str(other.data(), "status") != "review" {
		t.Fatalf("different amount: %s", other.Raw)
	}
	assertLedger(t)
}

func TestSMSValidationLinkDismissAndOwnership(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, ta, "A", "savings", 1000)
	for name, body := range map[string]map[string]any{
		"bad fingerprint": {"fingerprint": "abc"},
		"zero amount":     {"amount": 0},
		"bad direction":   {"direction": "sideways"},
		"long hint":       {"account_hint": "123456789"},
		"future":          {"received_at": time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)},
	} {
		if r := e.sms(t, ta, body); r.Status != 422 {
			t.Errorf("%s: want 422 got %d %s", name, r.Status, r.Raw)
		}
	}
	ev := e.sms(t, ta, map[string]any{"reference": "R9999999"})
	id := str(ev.data(), "id")
	for _, c := range []struct{ m, p string }{{"POST", "/v1/sms-events/" + id + "/dismiss"}, {"POST", "/v1/sms-events/" + id + "/link"}} {
		if r := e.call(t, c.m, c.p, tb, "", map[string]any{"transaction_id": a}); r.Status != 404 && r.Status != 422 {
			t.Errorf("%s by another user: %d", c.p, r.Status)
		}
	}
	if r := e.call(t, "POST", "/v1/sms-events/"+id+"/add", tb, newKey(), map[string]any{"account_id": a}); r.Status != 404 {
		t.Fatalf("add by another user: %d", r.Status)
	}
	if strings.Contains(e.do(t, "GET", "/v1/sms-events", tb, nil).Raw, id) {
		t.Fatal("review queue leaked")
	}
	if len(e.do(t, "GET", "/v1/sms-events", ta, nil).Body["data"].([]any)) != 1 {
		t.Fatal("review queue")
	}
	if r := e.call(t, "POST", "/v1/sms-events/"+id+"/dismiss", ta, "", nil); r.Status != 200 || str(r.data(), "status") != "dismissed" {
		t.Fatalf("dismiss: %d", r.Status)
	}
	if r := e.call(t, "POST", "/v1/sms-events/"+id+"/dismiss", ta, "", nil); r.Status != 409 {
		t.Fatalf("dismiss twice: %d", r.Status)
	}
	// Link to an existing transaction of your own.
	tx := str(e.spend(t, ta, a, 100, nil).data(), "id")
	ev2 := e.sms(t, ta, map[string]any{"reference": "R8888888", "amount": 5555})
	if r := e.call(t, "POST", "/v1/sms-events/"+str(ev2.data(), "id")+"/link", ta, "", map[string]any{"transaction_id": tx}); r.Status != 200 || str(r.data(), "status") != "linked" {
		t.Fatalf("link: %d %s", r.Status, r.Raw)
	}
	if countTx(t) != 1 {
		t.Fatal("linking creates nothing")
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSMSNeverListedTwiceAndLeavesTheListOnceHandled(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := str(e.call(t, "POST", "/v1/accounts", tok, newKey(), map[string]any{"name": "HDFC", "type": "savings", "opening_balance": 1000000, "masked_identifier": "4821"}).data(), "id")
	listed := func() []any { return e.do(t, "GET", "/v1/sms-events", tok, nil).Body["data"].([]any) }
	now := time.Now().UTC()
	// The bank texts twice about one payment (different text and time, same reference): one entry only.
	first := e.sms(t, tok, map[string]any{"amount": 30000, "merchant": "Cafe", "reference": "REF123456", "account_hint": "", "fingerprint": fp("a"), "received_at": now.Add(-2 * time.Hour).Format(time.RFC3339)})
	second := e.sms(t, tok, map[string]any{"amount": 30000, "merchant": "Cafe", "reference": "REF123456", "account_hint": "", "fingerprint": fp("b"), "received_at": now.Add(-time.Hour).Format(time.RFC3339)})
	if str(first.data(), "status") != "review" || str(second.data(), "status") != "duplicate" || len(listed()) != 1 {
		t.Fatalf("a second message about one payment must not be listed: %s / %s / %d listed", first.Raw, second.Raw, len(listed()))
	}
	// Re-sending the same message (a rescan) changes nothing.
	again := e.sms(t, tok, map[string]any{"amount": 30000, "merchant": "Cafe", "reference": "REF123456", "account_hint": "", "fingerprint": fp("a"), "received_at": now.Add(-2 * time.Hour).Format(time.RFC3339)})
	if len(listed()) != 1 || str(again.data(), "id") != str(first.data(), "id") {
		t.Fatalf("rescan: %s, %d listed", again.Raw, len(listed()))
	}
	// A message waiting in the list leaves it once the same movement is entered by hand.
	e.sms(t, tok, map[string]any{"amount": 77700, "merchant": "Book shop", "reference": "", "account_hint": "4821"})
	if len(listed()) != 2 {
		t.Fatalf("expected two to review, got %d", len(listed()))
	}
	e.spend(t, tok, a, 77700, map[string]any{"title": "Books"})
	if l := listed(); len(l) != 1 {
		t.Fatalf("a message matching a transaction added by hand must leave the list: %v", l)
	}
	// Added: gone from the list, and a rescan does not bring it back.
	id := str(listed()[0].(map[string]any), "id")
	if r := e.call(t, "POST", "/v1/sms-events/"+id+"/add", tok, newKey(), map[string]any{"account_id": a}); r.Status != 200 {
		t.Fatalf("add: %d %s", r.Status, r.Raw)
	}
	e.sms(t, tok, map[string]any{"amount": 30000, "merchant": "Cafe", "reference": "REF123456", "account_hint": "", "fingerprint": fp("a"), "received_at": now.Add(-2 * time.Hour).Format(time.RFC3339)})
	if len(listed()) != 0 {
		t.Fatalf("a handled message must not return: %v", listed())
	}
	assertLedger(t)
}
