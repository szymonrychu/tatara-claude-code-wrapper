package storage

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestConfigEnabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("empty config must be disabled")
	}
	if !(Config{Bucket: "b"}).Enabled() {
		t.Error("config with a bucket must be enabled")
	}
}

func TestNew_RequiresBucket(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Error("New must error without a bucket")
	}
}

func TestNew_BuildsClientWithEndpointAndStaticCreds(t *testing.T) {
	c, err := New(context.Background(), Config{
		Bucket:         "conv",
		Region:         "us-east-1",
		Endpoint:       "http://rook-ceph-rgw.tatara.svc",
		ForcePathStyle: true,
		AccessKeyID:    "AKIA_TEST",
		SecretKey:      "secret",
		KeyPrefix:      "tatara-conversations",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.bucket != "conv" || c.prefix != "tatara-conversations" {
		t.Errorf("client fields not set: bucket=%q prefix=%q", c.bucket, c.prefix)
	}
	if c.s3 == nil {
		t.Error("s3 client must be constructed")
	}
}

func TestFullKey(t *testing.T) {
	cases := []struct {
		prefix, key, want string
	}{
		{"", "issue-114/sid.jsonl", "issue-114/sid.jsonl"},
		{"tatara-conversations", "issue-114/sid.jsonl", "tatara-conversations/issue-114/sid.jsonl"},
		{"/tatara/", "/issue-114/sid.jsonl", "tatara/issue-114/sid.jsonl"},
		{"p", "k", "p/k"},
	}
	for _, tc := range cases {
		c := &Client{prefix: tc.prefix}
		if got := c.fullKey(tc.key); got != tc.want {
			t.Errorf("fullKey(prefix=%q,key=%q) = %q, want %q", tc.prefix, tc.key, got, tc.want)
		}
	}
}

// TestStore_RoundTrip exercises the Store interface end to end against the fake,
// the same surface the upload/restore hooks and operator GC reuse.
func TestStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	var s Store = NewMemStore()
	const key = "issue-114/abc.jsonl"

	ok, err := s.Exists(ctx, key)
	if err != nil || ok {
		t.Fatalf("Exists before Put = (%v,%v), want (false,nil)", ok, err)
	}
	if _, err := s.Get(ctx, key); err == nil {
		t.Error("Get of a missing key must error")
	}

	if err := s.Put(ctx, key, strings.NewReader("transcript-bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ok, err = s.Exists(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Exists after Put = (%v,%v), want (true,nil)", ok, err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "transcript-bytes" {
		t.Errorf("Get returned %q, want %q", got, "transcript-bytes")
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ok, _ = s.Exists(ctx, key)
	if ok {
		t.Error("Exists after Delete must be false")
	}
	// Deleting a missing key is a no-op, not an error.
	if err := s.Delete(ctx, key); err != nil {
		t.Errorf("Delete of missing key must be a no-op, got %v", err)
	}
}
