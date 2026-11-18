package storage

import (
	"strings"
	"testing"
	"time"
)

// AWS's documented SigV4 query-string example (https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html).
func TestSigV4MatchesAWSPublishedVector(t *testing.T) {
	in := signInput{
		Method: "GET", Host: "examplebucket.s3.amazonaws.com", Path: "/test.txt",
		Headers: map[string]string{"host": "examplebucket.s3.amazonaws.com"}, PayloadHash: "UNSIGNED-PAYLOAD",
		Region: "us-east-1", Service: "s3", AccessKey: "AKIAIOSFODNN7EXAMPLE", Secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Time: time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC),
	}
	q := presignedQuery(in, 86400*time.Second, nil)
	want := "X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(q, want) {
		t.Fatalf("signature mismatch:\n got  %s\n want ...%s", q, want)
	}
}

func TestPresignedPutSignsTypeAndExactSize(t *testing.T) {
	s, err := NewS3("https://acct.r2.cloudflarestorage.com", "bkt", "AK", "SK")
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC) }
	up, _ := s.PresignPut("u/1/a/2", "image/jpeg", 1000, 10*time.Minute)
	for _, must := range []string{"https://acct.r2.cloudflarestorage.com/bkt/u/1/a/2?", "X-Amz-Expires=600", "X-Amz-SignedHeaders=content-length%3Bcontent-type%3Bhost", "X-Amz-Signature="} {
		if !strings.Contains(up.URL, must) {
			t.Fatalf("missing %q in %s", must, up.URL)
		}
	}
	// A different size or type must produce a different signature.
	other, _ := s.PresignPut("u/1/a/2", "image/jpeg", 2000, 10*time.Minute)
	if up.URL == other.URL {
		t.Fatal("size is not part of the signature")
	}
	png, _ := s.PresignPut("u/1/a/2", "image/png", 1000, 10*time.Minute)
	if up.URL == png.URL {
		t.Fatal("content type is not part of the signature")
	}
}
