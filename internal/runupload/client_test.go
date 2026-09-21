package runupload

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
)

func TestS3CompatibleStoreUsesExplicitCredentialsAndContentAddressedKey(t *testing.T) {
	body := []byte("bundle bytes")
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/runs-bucket/team/roast/sha256/abc123.tar.zst" {
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "Credential=explicit-access/") || !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
			http.Error(w, "request was not signed with the explicit access key", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("X-Amz-Meta-Roast-Bundle-Sha256"); got != "abc123" {
			http.Error(w, "missing bundle digest metadata", http.StatusBadRequest)
			return
		}
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"etag-value"`)
		w.Header().Set("x-amz-version-id", "version-1")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store, err := NewStore(config.ResolvedUpload{
		Endpoint: server.URL, Bucket: "runs-bucket", Prefix: "team/roast", Region: "us-east-1",
		AccessKey: "explicit-access", SecretKey: "explicit-secret", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Put(t.Context(), bytes.NewReader(body), int64(len(body)), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("uploaded body = %q", gotBody)
	}
	if result.Key != "team/roast/sha256/abc123.tar.zst" || result.ETag != "etag-value" || result.VersionID != "version-1" {
		t.Fatalf("upload result = %#v", result)
	}
}
