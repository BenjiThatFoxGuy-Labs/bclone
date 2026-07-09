package gotohp

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/config/configmap"
)

func testAuthString() string {
	return "androidId=abc123&app=com.google.android.apps.photos&client_sig=sig&Email=test@example.com&Token=tok&lang=en_US&service=oauth2:https://www.googleapis.com/auth/photos.native"
}

// newTestFs builds a real *Fs (NewFs does no network I/O) for exercising
// pure logic — path routing and the phantom cache — without ever talking to
// Google.
func newTestFs(t *testing.T, root string) *Fs {
	t.Helper()
	m := configmap.Simple{"auth": testAuthString()}
	f, err := NewFs(context.Background(), "test", root, m)
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	gf, ok := f.(*Fs)
	if !ok {
		t.Fatalf("NewFs returned %T, want *Fs", f)
	}
	t.Cleanup(func() { _ = gf.Shutdown(context.Background()) })
	return gf
}

func TestResolvePathLoose(t *testing.T) {
	f := newTestFs(t, "")
	rp := f.resolvePath("photo.jpg")
	if rp.mode != albumNone {
		t.Errorf("mode = %v, want albumNone", rp.mode)
	}
	if rp.leaf != "photo.jpg" {
		t.Errorf("leaf = %q, want photo.jpg", rp.leaf)
	}
}

func TestResolvePathNewAlbum(t *testing.T) {
	f := newTestFs(t, "")
	rp := f.resolvePath("NewAlbum/Vacation/photo.jpg")
	if rp.mode != albumNew {
		t.Errorf("mode = %v, want albumNew", rp.mode)
	}
	if rp.albumRef != "Vacation" {
		t.Errorf("albumRef = %q, want Vacation", rp.albumRef)
	}
	if rp.leaf != "photo.jpg" {
		t.Errorf("leaf = %q, want photo.jpg", rp.leaf)
	}
}

func TestResolvePathExistingAlbum(t *testing.T) {
	f := newTestFs(t, "")
	rp := f.resolvePath("ExistingAlbum/AF1QipSomeID/nested/photo.jpg")
	if rp.mode != albumExisting {
		t.Errorf("mode = %v, want albumExisting", rp.mode)
	}
	if rp.albumRef != "AF1QipSomeID" {
		t.Errorf("albumRef = %q, want AF1QipSomeID", rp.albumRef)
	}
	if rp.leaf != "nested/photo.jpg" {
		t.Errorf("leaf = %q, want nested/photo.jpg", rp.leaf)
	}
}

func TestResolvePathRootPrefix(t *testing.T) {
	// Fs rooted at "NewAlbum/Vacation": Put is called with just "photo.jpg"
	// relative to that root, but routing must still see the full combined
	// path.
	f := newTestFs(t, "NewAlbum/Vacation")
	rp := f.resolvePath("photo.jpg")
	if rp.mode != albumNew {
		t.Errorf("mode = %v, want albumNew", rp.mode)
	}
	if rp.albumRef != "Vacation" {
		t.Errorf("albumRef = %q, want Vacation", rp.albumRef)
	}
	if rp.leaf != "photo.jpg" {
		t.Errorf("leaf = %q, want photo.jpg", rp.leaf)
	}
}

func TestResolvePathConfigAlbumOverridesRouting(t *testing.T) {
	f := newTestFs(t, "")
	f.opt.Album = "PinnedAlbum"
	rp := f.resolvePath("NewAlbum/Ignored/photo.jpg")
	if rp.mode != albumNew {
		t.Errorf("mode = %v, want albumNew", rp.mode)
	}
	if rp.albumRef != "PinnedAlbum" {
		t.Errorf("albumRef = %q, want PinnedAlbum (config album should win over path routing)", rp.albumRef)
	}
	if rp.leaf != "NewAlbum/Ignored/photo.jpg" {
		t.Errorf("leaf = %q, want the raw remote unparsed", rp.leaf)
	}
}

func TestPhantomCacheLifecycle(t *testing.T) {
	f := newTestFs(t, "")

	tmp, err := os.CreateTemp(f.phantomDir, "test-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := tmp.WriteString("hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tmp.Close()

	entry := &phantomEntry{
		size:      5,
		modTime:   time.Now(),
		sha1:      []byte{1, 2, 3},
		localPath: tmp.Name(),
		createdAt: time.Now(),
	}
	f.setPhantom("photo.jpg", entry)

	got, ok := f.getPhantom("photo.jpg")
	if !ok {
		t.Fatal("expected phantom entry to be present")
	}
	if got.size != 5 {
		t.Errorf("size = %d, want 5", got.size)
	}

	// NewObject should find it.
	obj, err := f.NewObject(context.Background(), "photo.jpg")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if obj.Size() != 5 {
		t.Errorf("obj.Size() = %d, want 5", obj.Size())
	}

	// Open should serve the real spooled bytes back.
	rc, err := obj.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(data, []byte("hello")) {
		t.Errorf("Open content = %q, want %q", data, "hello")
	}

	// removePhantom should delete both the map entry and the spooled file.
	f.removePhantom("photo.jpg")
	if _, ok := f.getPhantom("photo.jpg"); ok {
		t.Error("expected phantom entry to be gone after removePhantom")
	}
	if _, err := os.Stat(tmp.Name()); !os.IsNotExist(err) {
		t.Errorf("expected spooled file to be removed, stat err = %v", err)
	}

	// NewObject should now report not-found.
	if _, err := f.NewObject(context.Background(), "photo.jpg"); err == nil {
		t.Error("expected error for removed object")
	}
}

func TestSweepPhantomExpiry(t *testing.T) {
	f := newTestFs(t, "")
	f.opt.PhantomTTL = 0 // anything not "createdAt.Before(cutoff)" false immediately

	tmp, err := os.CreateTemp(f.phantomDir, "test-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	_ = tmp.Close()

	f.setPhantom("old.jpg", &phantomEntry{
		localPath: tmp.Name(),
		createdAt: time.Now().Add(-time.Hour),
	})

	f.sweepPhantom()

	if _, ok := f.getPhantom("old.jpg"); ok {
		t.Error("expected expired phantom entry to be swept")
	}
	if _, err := os.Stat(tmp.Name()); !os.IsNotExist(err) {
		t.Errorf("expected swept spooled file to be removed, stat err = %v", err)
	}
}

func TestListSynthesizesDirsAndObjects(t *testing.T) {
	f := newTestFs(t, "")
	now := time.Now()
	f.setPhantom("NewAlbum/Vacation/a.jpg", &phantomEntry{size: 1, modTime: now, createdAt: now})
	f.setPhantom("loose.jpg", &phantomEntry{size: 2, modTime: now, createdAt: now})

	entries, err := f.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var sawDir, sawObj bool
	for _, e := range entries {
		switch e.Remote() {
		case "NewAlbum":
			sawDir = true
		case "loose.jpg":
			sawObj = true
		}
	}
	if !sawDir {
		t.Error("expected a synthesized NewAlbum directory entry")
	}
	if !sawObj {
		t.Error("expected loose.jpg object entry")
	}
}

func TestMkdirRmdirAreNoOps(t *testing.T) {
	f := newTestFs(t, "")
	if err := f.Mkdir(context.Background(), "anything"); err != nil {
		t.Errorf("Mkdir: %v", err)
	}
	if err := f.Rmdir(context.Background(), "anything"); err != nil {
		t.Errorf("Rmdir: %v", err)
	}
}
