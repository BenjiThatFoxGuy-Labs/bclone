package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientPostsSendsAuthAndParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/posts.json", r.URL.Path)
		assert.Equal(t, "fav:me", r.URL.Query().Get("tags"))
		assert.Equal(t, "2", r.URL.Query().Get("page"))
		assert.Equal(t, "320", r.URL.Query().Get("limit"))

		user, pass, ok := r.BasicAuth()
		if assert.True(t, ok) {
			assert.Equal(t, "tester", user)
			assert.Equal(t, "key", pass)
		}
		assert.Contains(t, r.Header.Get("User-Agent"), "bclone")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PostsResponse{Posts: []Post{{
			ID:        1,
			CreatedAt: "2020-01-02T15:04:05.000-05:00",
			File:      File{Ext: "jpg", MD5: "deadbeef", Size: 123, URL: "https://static.example/deadbeef.jpg"},
		}}})
	}))
	defer srv.Close()

	c := NewClient(context.Background(), srv.Client(), srv.URL, "tester", "key")
	posts, err := c.Posts(context.Background(), "fav:me", 2, MaxPageSize)
	require.NoError(t, err)
	require.Len(t, posts, 1)
	assert.Equal(t, "deadbeef", posts[0].File.MD5)
}

func TestClientPostByMD5NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "md5:deadbeef", r.URL.Query().Get("tags"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PostsResponse{})
	}))
	defer srv.Close()

	c := NewClient(context.Background(), srv.Client(), srv.URL, "tester", "key")
	_, err := c.PostByMD5(context.Background(), "deadbeef")
	assert.ErrorIs(t, err, fs.ErrorObjectNotFound)
}

func TestClientCreatePost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// assert (not require): this runs on the server's own goroutine,
		// where FailNow/Goexit from require would be unsafe.
		assert.Equal(t, "/uploads.json", r.URL.Path)
		if assert.NoError(t, r.ParseMultipartForm(10<<20)) {
			file, header, err := r.FormFile("upload[file]")
			if assert.NoError(t, err) {
				defer func() { _ = file.Close() }()
				assert.Equal(t, "myfile.jpg", header.Filename)
				data, err := io.ReadAll(file)
				if assert.NoError(t, err) {
					assert.Equal(t, "hello world", string(data))
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer srv.Close()

	c := NewClient(context.Background(), srv.Client(), srv.URL, "tester", "key")
	err := c.CreatePost(context.Background(), "myfile.jpg", strings.NewReader("hello world"))
	require.NoError(t, err)
}
