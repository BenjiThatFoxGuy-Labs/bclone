package e621

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/e621/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testMD5 is a syntactically valid (32 hex char) stand-in for a real
// e621 file MD5, matching md5Leaf.
const testMD5 = "deadbeefdeadbeefdeadbeefdeadbeef"

func TestIsOfficialInstance(t *testing.T) {
	cases := []struct {
		endpoint string
		want     bool
	}{
		{"https://e621.net", true},
		{"https://e926.net", true},
		{"https://e6ai.net", true},
		{"https://www.e621.net", true},
		{"https://E621.NET", true},
		{"https://booru.example.com", false},
		{"https://e621.net.evil.example.com", false},
		{"://not a url", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, isOfficialInstance(c.endpoint), c.endpoint)
	}
}

func TestResolveTags(t *testing.T) {
	cases := []struct {
		full     string
		wantTags string
		wantOK   bool
	}{
		{"favorites", "fav:me", true},
		{"favs", "fav:me", true},
		{"recent", "", true},
		{"me", "user:me", true},
		{"search/rating:s canine", "rating:s canine", true},
		{"search", "", false},
		{"upload", "", false},
		{"unknown", "", false},
	}
	for _, c := range cases {
		tags, ok := resolveTags(c.full)
		assert.Equal(t, c.wantOK, ok, c.full)
		if ok {
			assert.Equal(t, c.wantTags, tags, c.full)
		}
	}
}

func TestMD5LeafPattern(t *testing.T) {
	assert.True(t, md5Leaf.MatchString("0123456789abcdef0123456789abcdef.jpg"))
	assert.False(t, md5Leaf.MatchString("not-an-md5.jpg"))
	assert.False(t, md5Leaf.MatchString("0123456789abcdef0123456789abcdef"))
}

// newTestFs builds a real *Fs against endpoint (or the official default if
// empty), with extra config values layered on top.
func newTestFs(t *testing.T, endpoint string, extra map[string]string) *Fs {
	t.Helper()
	m := configmap.Simple{
		"username": "tester",
		"api_key":  "key",
	}
	if endpoint != "" {
		m["endpoint"] = endpoint
	}
	for k, v := range extra {
		m[k] = v
	}
	fsObj, err := NewFs(context.Background(), "test", "", m)
	require.NoError(t, err)
	f, ok := fsObj.(*Fs)
	require.True(t, ok)
	return f
}

func TestUploadsAllowed(t *testing.T) {
	assert.False(t, newTestFs(t, "", nil).uploadsAllowed, "default endpoint is the official e621.net")
	assert.False(t, newTestFs(t, "https://e926.net", nil).uploadsAllowed, "e926.net is official")
	assert.False(t, newTestFs(t, "https://e6ai.net", nil).uploadsAllowed, "e6ai.net is official")
	assert.True(t, newTestFs(t, "https://booru.example.com", nil).uploadsAllowed, "custom endpoint allows uploads")
	assert.False(t, newTestFs(t, "https://booru.example.com", map[string]string{"disable_upload": "true"}).uploadsAllowed,
		"disable_upload forces it off even for a custom endpoint")
}

func rootNames(t *testing.T, f *Fs) []string {
	t.Helper()
	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Remote())
	}
	return names
}

func TestListRootHidesUploadDirWhenUnavailable(t *testing.T) {
	f := newTestFs(t, "", nil) // official endpoint: uploads disabled
	assert.ElementsMatch(t, []string{dirFavorites, dirFavs, dirRecent, dirMe}, rootNames(t, f))
}

func TestListRootShowsUploadDirWhenAvailable(t *testing.T) {
	f := newTestFs(t, "https://booru.example.com", nil)
	assert.ElementsMatch(t, []string{dirFavorites, dirFavs, dirRecent, dirMe, dirUpload}, rootNames(t, f))
}

func TestListUploadDirWhenHidden(t *testing.T) {
	f := newTestFs(t, "", nil)
	_, err := f.List(context.Background(), dirUpload)
	assert.ErrorIs(t, err, fs.ErrorDirNotFound)
}

func TestPutRejectsOutsideUploadDir(t *testing.T) {
	f := newTestFs(t, "https://booru.example.com", nil)
	src := object.NewStaticObjectInfo("favorites/foo.jpg", time.Now(), 4, true, nil, nil)
	_, err := f.Put(context.Background(), strings.NewReader("data"), src)
	assert.ErrorIs(t, err, fs.ErrorPermissionDenied)
}

func TestPutRejectsWhenUploadsDisabled(t *testing.T) {
	f := newTestFs(t, "", nil) // official endpoint
	src := object.NewStaticObjectInfo("upload/foo.jpg", time.Now(), 4, true, nil, nil)
	_, err := f.Put(context.Background(), strings.NewReader("data"), src)
	assert.ErrorIs(t, err, fs.ErrorPermissionDenied)
}

func TestPutUploadsIntoUploadDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// assert (not require): this runs on the server's own goroutine,
		// where FailNow/Goexit from require would be unsafe.
		assert.Equal(t, "/uploads.json", r.URL.Path)
		if assert.NoError(t, r.ParseMultipartForm(10<<20)) {
			file, header, err := r.FormFile("upload[file]")
			if assert.NoError(t, err) {
				defer func() { _ = file.Close() }()
				assert.Equal(t, "foo.jpg", header.Filename)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer srv.Close()

	f := newTestFs(t, srv.URL, nil)
	modTime := time.Now()
	src := object.NewStaticObjectInfo("upload/foo.jpg", modTime, int64(len("hello world")), true, nil, nil)
	obj, err := f.Put(context.Background(), strings.NewReader("hello world"), src)
	require.NoError(t, err)
	assert.Equal(t, "upload/foo.jpg", obj.Remote())
	assert.Equal(t, int64(len("hello world")), obj.Size())

	sum, err := obj.Hash(context.Background(), hash.MD5)
	require.NoError(t, err)
	assert.NotEmpty(t, sum)
}

// postsHandler serves /posts.json, returning posts only for the expected
// tags, and md5:<md5> lookups for any md5 among them.
func postsHandler(t *testing.T, expectTags string, posts []api.Post) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		tags := r.URL.Query().Get("tags")
		w.Header().Set("Content-Type", "application/json")
		if tags == expectTags {
			_ = json.NewEncoder(w).Encode(api.PostsResponse{Posts: posts})
			return
		}
		for _, p := range posts {
			if tags == "md5:"+p.File.MD5 {
				_ = json.NewEncoder(w).Encode(api.PostsResponse{Posts: []api.Post{p}})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(api.PostsResponse{})
	}
}

func TestListFavorites(t *testing.T) {
	posts := []api.Post{{
		ID:        1,
		CreatedAt: "2020-01-02T15:04:05.000-05:00",
		File:      api.File{Ext: "jpg", MD5: testMD5, Size: 42, URL: "https://static.example/" + testMD5 + ".jpg"},
	}}
	srv := httptest.NewServer(postsHandler(t, "fav:me", posts))
	defer srv.Close()

	f := newTestFs(t, srv.URL, nil)
	for _, dir := range []string{dirFavorites, dirFavs} {
		entries, err := f.List(context.Background(), dir)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, dir+"/"+testMD5+".jpg", entries[0].Remote())
	}
}

func TestNewObjectDirectFetch(t *testing.T) {
	posts := []api.Post{{
		ID:        1,
		CreatedAt: "2020-01-02T15:04:05.000-05:00",
		File:      api.File{Ext: "jpg", MD5: testMD5, Size: 42, URL: "https://static.example/" + testMD5 + ".jpg"},
	}}
	srv := httptest.NewServer(postsHandler(t, "", posts))
	defer srv.Close()

	f := newTestFs(t, srv.URL, nil)
	require.True(t, f.directFetch, "not running under mount/serve in this test")

	obj, err := f.NewObject(context.Background(), testMD5+".jpg")
	require.NoError(t, err)
	assert.Equal(t, int64(42), obj.Size())
}

func TestNewObjectDirectFetchExcludedUnderMountOrServe(t *testing.T) {
	prevMode := vfscommon.Opt.CacheMode
	vfscommon.Opt.CacheMode = vfscommon.CacheModeWrites
	t.Cleanup(func() { vfscommon.Opt.CacheMode = prevMode })

	posts := []api.Post{{
		ID:        1,
		CreatedAt: "2020-01-02T15:04:05.000-05:00",
		File:      api.File{Ext: "jpg", MD5: testMD5, Size: 42, URL: "https://static.example/" + testMD5 + ".jpg"},
	}}
	srv := httptest.NewServer(postsHandler(t, "", posts))
	defer srv.Close()

	f := newTestFs(t, srv.URL, nil)
	assert.False(t, f.directFetch)

	_, err := f.NewObject(context.Background(), testMD5+".jpg")
	assert.ErrorIs(t, err, fs.ErrorObjectNotFound)

	// Still reachable once it's within a known virtual directory.
	favSrv := httptest.NewServer(postsHandler(t, "fav:me", posts))
	defer favSrv.Close()
	f2 := newTestFs(t, favSrv.URL, nil)
	obj, err := f2.NewObject(context.Background(), "favorites/"+testMD5+".jpg")
	require.NoError(t, err)
	assert.Equal(t, int64(42), obj.Size())
}
