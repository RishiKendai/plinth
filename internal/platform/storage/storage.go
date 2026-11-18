// Package storage talks to S3-compatible object storage (Cloudflare R2). Objects are private: the API never exposes
// a permanent URL, only short-lived presigned ones (CLAUDE.md 2.4). There is no SDK: signing is plain SigV4 so the
// dependency surface stays small, and the signer is checked against AWS's published test vector.
package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound means the object does not exist.
var ErrNotFound = errors.New("storage: object not found")

type ObjectInfo struct {
	Size        int64
	ContentType string
}

// Upload describes a presigned upload. The client must send exactly these headers and exactly Size bytes.
type Upload struct {
	URL     string
	Headers map[string]string
	Expires time.Time
}

// Store is what the attachments module needs. Tests use an in-memory implementation; production uses S3.
type Store interface {
	PresignPut(key, contentType string, size int64, ttl time.Duration) (Upload, error)
	PresignGet(key, downloadName string, ttl time.Duration) (string, time.Time, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Range(ctx context.Context, key string, n int64) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// S3 is a path-style S3 client for R2: https://<account>.r2.cloudflarestorage.com/<bucket>/<key>.
type S3 struct {
	Endpoint  *url.URL
	Bucket    string
	Region    string // "auto" for R2
	AccessKey string
	SecretKey string
	HTTP      *http.Client
	Now       func() time.Time
}

func NewS3(endpoint, bucket, accessKey, secretKey string) (*S3, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage: invalid endpoint")
	}
	return &S3{Endpoint: u, Bucket: bucket, Region: "auto", AccessKey: accessKey, SecretKey: secretKey,
		HTTP: &http.Client{Timeout: 20 * time.Second}, Now: time.Now}, nil
}

func (s *S3) path(key string) string { return "/" + s.Bucket + "/" + key }

// ---- SigV4 ----

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// uriEncode encodes per the SigV4 rules: unreserved characters stay, everything else is %XX. Slashes in a path stay.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

type signInput struct {
	Method, Host, Path string
	Query              map[string]string // excluding X-Amz-Signature
	Headers            map[string]string // lower-case names; must include host
	PayloadHash        string
	Region, Service    string
	AccessKey, Secret  string
	Time               time.Time
}

func (in signInput) signedHeaderNames() string {
	names := make([]string, 0, len(in.Headers))
	for k := range in.Headers {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ";")
}

func (in signInput) canonicalRequest() string {
	qk := make([]string, 0, len(in.Query))
	for k := range in.Query {
		qk = append(qk, k)
	}
	sort.Strings(qk)
	parts := make([]string, len(qk))
	for i, k := range qk {
		parts[i] = uriEncode(k, false) + "=" + uriEncode(in.Query[k], false)
	}
	hk := make([]string, 0, len(in.Headers))
	for k := range in.Headers {
		hk = append(hk, k)
	}
	sort.Strings(hk)
	var hb strings.Builder
	for _, k := range hk {
		hb.WriteString(k + ":" + strings.TrimSpace(in.Headers[k]) + "\n")
	}
	return strings.Join([]string{in.Method, uriEncode(in.Path, true), strings.Join(parts, "&"), hb.String(), in.signedHeaderNames(), in.PayloadHash}, "\n")
}

func (in signInput) signature() string {
	date := in.Time.UTC().Format("20060102")
	scope := date + "/" + in.Region + "/" + in.Service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + in.Time.UTC().Format("20060102T150405Z") + "\n" + scope + "\n" + sha256Hex([]byte(in.canonicalRequest()))
	k := hmacSHA256([]byte("AWS4"+in.Secret), date)
	k = hmacSHA256(k, in.Region)
	k = hmacSHA256(k, in.Service)
	k = hmacSHA256(k, "aws4_request")
	return hex.EncodeToString(hmacSHA256(k, toSign))
}

// presignedQuery builds the full signed query string for a presigned URL.
func presignedQuery(in signInput, expires time.Duration, extra map[string]string) string {
	date := in.Time.UTC().Format("20060102")
	q := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    in.AccessKey + "/" + date + "/" + in.Region + "/" + in.Service + "/aws4_request",
		"X-Amz-Date":          in.Time.UTC().Format("20060102T150405Z"),
		"X-Amz-Expires":       strconv.Itoa(int(expires.Seconds())),
		"X-Amz-SignedHeaders": in.signedHeaderNames(),
	}
	for k, v := range extra {
		q[k] = v
	}
	in.Query = q
	sig := in.signature()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		parts = append(parts, uriEncode(k, false)+"="+uriEncode(q[k], false))
	}
	parts = append(parts, "X-Amz-Signature="+sig)
	return strings.Join(parts, "&")
}

func (s *S3) base(method, key string, headers map[string]string, t time.Time) signInput {
	h := map[string]string{"host": s.Endpoint.Host}
	for k, v := range headers {
		h[strings.ToLower(k)] = v
	}
	return signInput{Method: method, Host: s.Endpoint.Host, Path: s.path(key), Headers: h, PayloadHash: "UNSIGNED-PAYLOAD",
		Region: s.Region, Service: "s3", AccessKey: s.AccessKey, Secret: s.SecretKey, Time: t}
}

// PresignPut returns a URL that accepts exactly one upload of exactly `size` bytes with the given content type.
// Both are part of the signature, so a client cannot upload a larger file or a different type with this URL.
func (s *S3) PresignPut(key, contentType string, size int64, ttl time.Duration) (Upload, error) {
	t := s.Now()
	in := s.base("PUT", key, map[string]string{"content-type": contentType, "content-length": strconv.FormatInt(size, 10)}, t)
	u := *s.Endpoint
	u.Path, u.RawPath = "", ""
	full := u.String() + uriEncode(in.Path, true) + "?" + presignedQuery(in, ttl, nil)
	return Upload{URL: full, Headers: map[string]string{"Content-Type": contentType}, Expires: t.Add(ttl)}, nil
}

// PresignGet returns a short-lived download URL. downloadName sets the filename the browser offers.
func (s *S3) PresignGet(key, downloadName string, ttl time.Duration) (string, time.Time, error) {
	t := s.Now()
	in := s.base("GET", key, nil, t)
	extra := map[string]string{}
	if downloadName != "" {
		extra["response-content-disposition"] = `inline; filename="` + strings.NewReplacer(`"`, "", "\\", "", "\r", "", "\n", "").Replace(downloadName) + `"`
	}
	u := *s.Endpoint
	u.Path, u.RawPath = "", ""
	return u.String() + uriEncode(in.Path, true) + "?" + presignedQuery(in, ttl, extra), t.Add(ttl), nil
}

// do sends a header-signed request (server to R2).
func (s *S3) do(ctx context.Context, method, key string, extra map[string]string) (*http.Response, error) {
	t := s.Now()
	empty := sha256Hex(nil)
	hdr := map[string]string{"x-amz-content-sha256": empty, "x-amz-date": t.UTC().Format("20060102T150405Z")}
	for k, v := range extra {
		hdr[strings.ToLower(k)] = v
	}
	in := s.base(method, key, hdr, t)
	in.PayloadHash = empty
	auth := "AWS4-HMAC-SHA256 Credential=" + s.AccessKey + "/" + t.UTC().Format("20060102") + "/" + s.Region + "/s3/aws4_request, SignedHeaders=" +
		in.signedHeaderNames() + ", Signature=" + in.signature()
	u := *s.Endpoint
	u.Path, u.RawPath = "", ""
	req, err := http.NewRequestWithContext(ctx, method, u.String()+uriEncode(in.Path, true), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", auth)
	return s.HTTP.Do(req)
}

func (s *S3) Head(ctx context.Context, key string) (ObjectInfo, error) {
	res, err := s.do(ctx, http.MethodHead, key, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return ObjectInfo{Size: res.ContentLength, ContentType: res.Header.Get("Content-Type")}, nil
	case http.StatusNotFound:
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{}, fmt.Errorf("storage: head returned %d", res.StatusCode)
}

// Range reads the first n bytes of an object, used to sniff the real file type.
func (s *S3) Range(ctx context.Context, key string, n int64) ([]byte, error) {
	res, err := s.do(ctx, http.MethodGet, key, map[string]string{"range": "bytes=0-" + strconv.FormatInt(n-1, 10)})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("storage: get returned %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, n))
}

func (s *S3) Delete(ctx context.Context, key string) error {
	res, err := s.do(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotFound {
		return fmt.Errorf("storage: delete returned %d", res.StatusCode)
	}
	return nil
}
