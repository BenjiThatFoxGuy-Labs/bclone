package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAuthString() string {
	return "androidId=abc123&app=com.google.android.apps.photos&client_sig=sig&Email=test@example.com&Token=tok&lang=en_US&service=oauth2:https://www.googleapis.com/auth/photos.native"
}

// newTestClient builds a Client whose endpoints all point at an httptest
// server backed by mux, so the real Google endpoints are never touched.
func newTestClient(t *testing.T, mux *http.ServeMux) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(mux)
	c, err := NewClient(server.Client(), testAuthString(), QualityOriginal, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.authEndpoint = server.URL + "/auth"
	c.uploadEndpoint = server.URL + "/upload"
	c.hashCheckEndpoint = server.URL + "/hashcheck"
	c.commitEndpoint = server.URL + "/commit"
	c.createAlbumEndpoint = server.URL + "/createalbum"
	c.addToAlbumEndpoint = server.URL + "/addtoalbum"
	return c, server
}

func fakeAuthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Auth=faketoken\nExpiry=%d\n", time.Now().Unix()+3600)
}

func TestParseAuthMissingFields(t *testing.T) {
	_, err := ParseAuth("androidId=abc&app=x")
	if err == nil {
		t.Fatal("expected error for missing required fields")
	}
}

func TestParseAuthValid(t *testing.T) {
	params, err := ParseAuth(testAuthString())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if params.Get("Email") != "test@example.com" {
		t.Errorf("Email = %q", params.Get("Email"))
	}
}

func TestGetAuthTokenAndBearerCaching(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.FormValue("app"); got != "com.google.android.apps.photos" {
			t.Errorf("app = %q", got)
		}
		if got := r.Header.Get("device"); got != "abc123" {
			t.Errorf("device header = %q, want abc123", got)
		}
		fakeAuthHandler(w, r)
	})
	c, server := newTestClient(t, mux)
	defer server.Close()

	tok1, err := c.bearerToken(context.Background())
	if err != nil {
		t.Fatalf("bearerToken: %v", err)
	}
	if tok1 != "faketoken" {
		t.Errorf("token = %q, want faketoken", tok1)
	}
	tok2, err := c.bearerToken(context.Background())
	if err != nil {
		t.Fatalf("bearerToken (cached): %v", err)
	}
	if tok2 != "faketoken" {
		t.Errorf("cached token = %q, want faketoken", tok2)
	}
	if calls != 1 {
		t.Errorf("auth endpoint called %d times, want 1 (should be cached)", calls)
	}
}

func TestGetAuthTokenBindingRequired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Error=NeedsBrowser\n")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	authWithBinding := testAuthString() + "&token_binding_alias=somealias"
	c, err := NewClient(server.Client(), authWithBinding, QualityOriginal, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.authEndpoint = server.URL + "/auth"

	_, err = c.bearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "token binding") {
		t.Errorf("error = %v, want mention of token binding", err)
	}
}

func TestGetUploadToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", fakeAuthHandler)
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer faketoken" {
			t.Errorf("Authorization = %q", got)
		}
		wantHash := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
		if got := r.Header.Get("X-Goog-Hash"); got != "sha1="+wantHash {
			t.Errorf("X-Goog-Hash = %q, want sha1=%s", got, wantHash)
		}
		if got := r.Header.Get("X-Upload-Content-Length"); got != "42" {
			t.Errorf("X-Upload-Content-Length = %q, want 42", got)
		}
		w.Header().Set("X-GUploader-UploadID", "upload-token-xyz")
		w.WriteHeader(http.StatusOK)
	})
	c, server := newTestClient(t, mux)
	defer server.Close()

	token, err := c.GetUploadToken(context.Background(), []byte{1, 2, 3}, 42)
	if err != nil {
		t.Fatalf("GetUploadToken: %v", err)
	}
	if token != "upload-token-xyz" {
		t.Errorf("token = %q, want upload-token-xyz", token)
	}
}

func TestGetUploadTokenMissingHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", fakeAuthHandler)
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	c, server := newTestClient(t, mux)
	defer server.Close()

	_, err := c.GetUploadToken(context.Background(), []byte{1}, 1)
	if err == nil {
		t.Fatal("expected error for missing X-GUploader-UploadID header")
	}
}

func TestUploadBytes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", fakeAuthHandler)
	var gotBody []byte
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if got := r.URL.Query().Get("upload_id"); got != "tok-123" {
			t.Errorf("upload_id = %q, want tok-123", got)
		}
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var b pbBuilder
		b = b.varint(1, 55)
		b = b.bytes(2, []byte("committoken"))
		_, _ = w.Write(b)
	})
	c, server := newTestClient(t, mux)
	defer server.Close()

	content := []byte("hello world")
	tok, err := c.UploadBytes(context.Background(), "tok-123", func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(content))), nil
	})
	if err != nil {
		t.Fatalf("UploadBytes: %v", err)
	}
	if !bytes.Equal(gotBody, content) {
		t.Errorf("uploaded body = %q, want %q", gotBody, content)
	}
	if tok.Field1 != 55 || string(tok.Field2) != "committoken" {
		t.Errorf("CommitToken = %+v", tok)
	}
}

func TestCommitUpload(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", fakeAuthHandler)
	mux.HandleFunc("/commit", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Goog-Ext-173412678-Bin"); got != "CgcIAhClARgC" {
			t.Errorf("X-Goog-Ext-173412678-Bin = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		f, err := parsePBFields(body)
		if err != nil {
			t.Fatalf("parse request body: %v", err)
		}
		if got := f.msg(1).strAt(2); got != "photo.jpg" {
			t.Errorf("file_name in request = %q, want photo.jpg", got)
		}
		var mkMsg pbBuilder
		mkMsg = mkMsg.str(1, "AF1QipCommitted")
		var field1 pbBuilder
		field1 = field1.message(3, mkMsg)
		var resp pbBuilder
		resp = resp.message(1, field1)
		_, _ = w.Write(resp)
	})
	c, server := newTestClient(t, mux)
	defer server.Close()

	mk, err := c.CommitUpload(context.Background(), CommitToken{Field1: 1, Field2: []byte("x")}, "photo.jpg", []byte{1, 2, 3}, 1700000000)
	if err != nil {
		t.Fatalf("CommitUpload: %v", err)
	}
	if mk != "AF1QipCommitted" {
		t.Errorf("mediaKey = %q, want AF1QipCommitted", mk)
	}
}

func TestFindRemoteMediaByHash(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", fakeAuthHandler)
	mux.HandleFunc("/hashcheck", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Goog-Ext-173412678-Bin"); got != "" {
			t.Errorf("hash check should not set ext headers, got %q", got)
		}
		var mkMsg pbBuilder
		mkMsg = mkMsg.str(1, "AF1QipExisting")
		var inner pbBuilder
		inner = inner.message(2, mkMsg)
		var f1 pbBuilder
		f1 = f1.message(2, inner)
		var top pbBuilder
		top = top.message(1, f1)
		_, _ = w.Write(top)
	})
	c, server := newTestClient(t, mux)
	defer server.Close()

	mk, err := c.FindRemoteMediaByHash(context.Background(), []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("FindRemoteMediaByHash: %v", err)
	}
	if mk != "AF1QipExisting" {
		t.Errorf("mediaKey = %q, want AF1QipExisting", mk)
	}
}

func TestCreateAlbum(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", fakeAuthHandler)
	mux.HandleFunc("/createalbum", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f, err := parsePBFields(body)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got := f.strAt(1); got != "Vacation" {
			t.Errorf("album_name = %q, want Vacation", got)
		}
		var field1 pbBuilder
		field1 = field1.str(1, "AF1QipNewAlbum")
		var resp pbBuilder
		resp = resp.message(1, field1)
		_, _ = w.Write(resp)
	})
	c, server := newTestClient(t, mux)
	defer server.Close()

	id, err := c.CreateAlbum(context.Background(), "Vacation", []string{"mk1"})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	if id != "AF1QipNewAlbum" {
		t.Errorf("albumID = %q, want AF1QipNewAlbum", id)
	}
}

func TestAddMediaToAlbum(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", fakeAuthHandler)
	called := false
	mux.HandleFunc("/addtoalbum", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	c, server := newTestClient(t, mux)
	defer server.Close()

	if err := c.AddMediaToAlbum(context.Background(), "AF1QipExisting", []string{"mk1"}); err != nil {
		t.Fatalf("AddMediaToAlbum: %v", err)
	}
	if !called {
		t.Error("addtoalbum endpoint not called")
	}
}

func TestPostProtobufErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", fakeAuthHandler)
	mux.HandleFunc("/hashcheck", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server exploded", http.StatusInternalServerError)
	})
	c, server := newTestClient(t, mux)
	defer server.Close()

	_, err := c.FindRemoteMediaByHash(context.Background(), []byte{1})
	if err == nil {
		t.Fatal("expected error")
	}
}
