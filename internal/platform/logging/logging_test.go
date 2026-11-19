package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, "debug", true)
	l.Info("x", "password", "hunter2", "refresh_token", "abc", "Authorization", "Bearer z", "user_id", "u-1", "request_id", "r-1")
	out := b.String()
	for _, leaked := range []string{"hunter2", "abc", "Bearer z"} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %q in %s", leaked, out)
		}
	}
	for _, kept := range []string{"u-1", "r-1"} {
		if !strings.Contains(out, kept) {
			t.Errorf("over-redacted %q in %s", kept, out)
		}
	}
}

// Phase 4-7 added presigned URLs, bank message fingerprints and report content. None may reach a log line.
func TestRedactionCoversCaptureAndReportData(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, "debug", true)
	l.Info("x", "upload_url", "https://r2/u/1?X-Amz-Signature=sig", "sms_body", "Rs 5 debited A/c XX1234", "fingerprint", "deadbeef", "report_content", "Date,Title", "presigned", "p", "attachment_id", "a-1")
	out := b.String()
	for _, leaked := range []string{"X-Amz-Signature", "debited", "deadbeef", "Date,Title"} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %q in %s", leaked, out)
		}
	}
	if !strings.Contains(out, "a-1") {
		t.Errorf("ids must stay visible: %s", out)
	}
}
