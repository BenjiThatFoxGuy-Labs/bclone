package gotohp

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/vfs/vfscommon"
)

func testAuthString() string {
	return "androidId=abc123&app=com.google.android.apps.photos&client_sig=sig&Email=test@example.com&Token=tok&lang=en_US&service=oauth2:https://www.googleapis.com/auth/photos.native"
}

// newTestFs builds a real *Fs (NewFs does no network I/O) with deferred
// uploads active, simulating what NewFs auto-detects when running under
// mount/serve (--vfs-cache-mode writes or full) — see NewFs's use of
// vfscommon.Opt. Used by tests exercising the phantom-cache mechanism
// itself; production toggling of deferUploads is covered separately by
// TestDeferUploadsDefaultIsFalse / TestDeferUploadsAutoDetectedFromVFSCacheMode.
func newTestFs(t *testing.T, root string) *Fs {
	t.Helper()
	prevMode := vfscommon.Opt.CacheMode
	prevWriteBack := vfscommon.Opt.WriteBack
	vfscommon.Opt.CacheMode = vfscommon.CacheModeWrites
	vfscommon.Opt.WriteBack = fs.Duration(5 * time.Minute)
	t.Cleanup(func() {
		vfscommon.Opt.CacheMode = prevMode
		vfscommon.Opt.WriteBack = prevWriteBack
	})

	m := configmap.Simple{"auth": testAuthString()}
	f, err := NewFs(context.Background(), "test", root, m)
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	gf, ok := f.(*Fs)
	if !ok {
		t.Fatalf("NewFs returned %T, want *Fs", f)
	}
	if !gf.deferUploads {
		t.Fatal("expected deferUploads to be auto-detected as true with vfscommon.Opt.CacheMode = writes")
	}
	t.Cleanup(func() { _ = gf.Shutdown(context.Background()) })
	return gf
}

// newSyncTestFs builds a real *Fs with deferred uploads left off, matching
// a plain copy/sync/move process where vfscommon.Opt.CacheMode is never
// touched (stays at its Go zero value, CacheModeOff).
func newSyncTestFs(t *testing.T, root string) *Fs {
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
	f.lingerFor = 0 // anything not "createdAt.Before(cutoff)" false immediately

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

func TestDeferUploadsDefaultIsFalse(t *testing.T) {
	// A plain copy/sync/move process never touches --vfs-cache-mode, so
	// vfscommon.Opt.CacheMode stays at its Go zero value (CacheModeOff) —
	// exactly what a fresh test binary also has, with no mutation needed.
	gf := newSyncTestFs(t, "")
	if gf.deferUploads {
		t.Error("deferUploads should default to false when --vfs-cache-mode was never set (synchronous mode)")
	}
}

func TestDeferUploadsAutoDetectedFromVFSCacheMode(t *testing.T) {
	// This is exactly what newTestFs sets up; assert it explicitly here too
	// so the auto-detection wiring in NewFs itself has a direct test.
	gf := newTestFs(t, "")
	if !gf.deferUploads {
		t.Error("deferUploads should be true when vfscommon.Opt.CacheMode is not off")
	}
	if gf.lingerFor != 5*time.Minute {
		t.Errorf("lingerFor = %v, want 5m (from vfscommon.Opt.WriteBack)", gf.lingerFor)
	}
}

func TestSyncModeIgnoresPhantomCache(t *testing.T) {
	gf := newSyncTestFs(t, "")

	if gf.deferUploads {
		t.Fatal("deferUploads should be false in sync mode")
	}

	// Even if something ended up in the phantom map, sync mode must not
	// surface it via List/NewObject/Open — those should behave as a plain
	// write-only remote with nothing lingering.
	gf.setPhantom("photo.jpg", &phantomEntry{size: 1, modTime: time.Now(), createdAt: time.Now()})

	entries, err := gf.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("List returned %d entries in sync mode, want 0", len(entries))
	}

	if _, err := gf.NewObject(context.Background(), "photo.jpg"); err != fs.ErrorObjectNotFound {
		t.Errorf("NewObject error = %v, want fs.ErrorObjectNotFound", err)
	}

	obj := &Object{f: gf, remote: "photo.jpg", size: 1, modTime: time.Now()}
	if _, err := obj.Open(context.Background()); err == nil {
		t.Error("expected Open to fail in sync mode")
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
