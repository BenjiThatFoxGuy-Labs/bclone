package api

import (
	"bytes"
	"testing"
)

func TestEncodeGetUploadToken(t *testing.T) {
	data := EncodeGetUploadToken(12345)
	f, err := parsePBFields(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := f.varintAt(1); got != 2 {
		t.Errorf("f1 = %d, want 2", got)
	}
	if got := f.varintAt(2); got != 2 {
		t.Errorf("f2 = %d, want 2", got)
	}
	if got := f.varintAt(3); got != 1 {
		t.Errorf("f3 = %d, want 1", got)
	}
	if got := f.varintAt(4); got != 3 {
		t.Errorf("f4 = %d, want 3", got)
	}
	if got := f.varintAt(7); got != 12345 {
		t.Errorf("file_size_bytes = %d, want 12345", got)
	}
}

func TestEncodeHashCheck(t *testing.T) {
	sha1Hash := []byte{1, 2, 3, 4, 5}
	data := EncodeHashCheck(sha1Hash)
	f, err := parsePBFields(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := f.msg(1).msg(1).bytesAt(1)
	if !bytes.Equal(got, sha1Hash) {
		t.Errorf("sha1Hash = %x, want %x", got, sha1Hash)
	}
}

func TestDecodeRemoteMatchesMediaKey(t *testing.T) {
	var mkMsg pbBuilder
	mkMsg = mkMsg.str(1, "AF1QipTest")
	var innerField2 pbBuilder
	innerField2 = innerField2.message(2, mkMsg)
	var field2 pbBuilder
	field2 = field2.message(2, innerField2)
	var top pbBuilder
	top = top.message(1, field2)

	mk, err := DecodeRemoteMatchesMediaKey(top)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mk != "AF1QipTest" {
		t.Errorf("mediaKey = %q, want %q", mk, "AF1QipTest")
	}
}

func TestDecodeRemoteMatchesMediaKeyEmpty(t *testing.T) {
	mk, err := DecodeRemoteMatchesMediaKey(nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mk != "" {
		t.Errorf("mediaKey = %q, want empty", mk)
	}
}

func TestCommitTokenDecode(t *testing.T) {
	var b pbBuilder
	b = b.varint(1, 42)
	b = b.bytes(2, []byte{9, 8, 7})
	tok, err := DecodeCommitToken(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tok.Field1 != 42 {
		t.Errorf("Field1 = %d, want 42", tok.Field1)
	}
	if !bytes.Equal(tok.Field2, []byte{9, 8, 7}) {
		t.Errorf("Field2 = %v, want [9 8 7]", tok.Field2)
	}
}

func TestEncodeCommitUpload(t *testing.T) {
	token := CommitToken{Field1: 7, Field2: []byte("xyz")}
	data := EncodeCommitUpload(token, "photo.jpg", []byte{1, 2, 3}, 1234567890, 3, "Pixel XL", "Google", 28)
	f, err := parsePBFields(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f1 := f.msg(1)
	if f1 == nil {
		t.Fatal("field1 missing")
	}
	if got := f1.strAt(2); got != "photo.jpg" {
		t.Errorf("file_name = %q, want photo.jpg", got)
	}
	if got := f1.bytesAt(3); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Errorf("sha1_hash = %v, want [1 2 3]", got)
	}
	if got := f1.varintAt(7); got != 3 {
		t.Errorf("quality = %d, want 3", got)
	}
	if got := f1.varintAt(10); got != 1 {
		t.Errorf("field10 = %d, want 1", got)
	}
	tokenMsg := f1.msg(1)
	if got := tokenMsg.varintAt(1); got != 7 {
		t.Errorf("token field1 = %d, want 7", got)
	}
	if got := tokenMsg.bytesAt(2); !bytes.Equal(got, []byte("xyz")) {
		t.Errorf("token field2 = %q, want xyz", got)
	}
	mtimeMsg := f1.msg(4)
	if got := mtimeMsg.varintAt(1); got != 1234567890 {
		t.Errorf("file_last_modified_timestamp = %d, want 1234567890", got)
	}
	f2 := f.msg(2)
	if got := f2.strAt(3); got != "Pixel XL" {
		t.Errorf("model = %q, want Pixel XL", got)
	}
	if got := f2.strAt(4); got != "Google" {
		t.Errorf("make = %q, want Google", got)
	}
	if got := f2.varintAt(5); got != 28 {
		t.Errorf("android_api_version = %d, want 28", got)
	}
	if got := f.bytesAt(3); !bytes.Equal(got, []byte{1, 3}) {
		t.Errorf("field3 = %v, want [1 3]", got)
	}
}

func TestDecodeCommitUploadResponseMediaKey(t *testing.T) {
	var mediaKeyMsg pbBuilder
	mediaKeyMsg = mediaKeyMsg.str(1, "AF1QipMediaKey")
	var field1 pbBuilder
	field1 = field1.message(3, mediaKeyMsg)
	var top pbBuilder
	top = top.message(1, field1)

	mk, err := DecodeCommitUploadResponseMediaKey(top)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mk != "AF1QipMediaKey" {
		t.Errorf("mediaKey = %q, want AF1QipMediaKey", mk)
	}
}

func TestDecodeCommitUploadResponseMediaKeyMissing(t *testing.T) {
	_, err := DecodeCommitUploadResponseMediaKey(nil)
	if err == nil {
		t.Fatal("expected error for missing media key")
	}
}

func TestEncodeCreateAlbum(t *testing.T) {
	data := EncodeCreateAlbum("My Album", []string{"mk1", "mk2"}, 1700000000, "Pixel XL", "Google", 28)
	f, err := parsePBFields(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := f.strAt(1); got != "My Album" {
		t.Errorf("album_name = %q, want My Album", got)
	}
	if got := f.varintAt(2); got != 1700000000 {
		t.Errorf("timestamp = %d, want 1700000000", got)
	}
	entries := f[4]
	if len(entries) != 2 {
		t.Fatalf("media_keys entries = %d, want 2", len(entries))
	}
	device := f.msg(8)
	if got := device.strAt(3); got != "Pixel XL" {
		t.Errorf("device model = %q, want Pixel XL", got)
	}
}

func TestDecodeCreateAlbumResponseAlbumMediaKey(t *testing.T) {
	var field1 pbBuilder
	field1 = field1.str(1, "AF1QipAlbumKey")
	var top pbBuilder
	top = top.message(1, field1)

	amk, err := DecodeCreateAlbumResponseAlbumMediaKey(top)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if amk != "AF1QipAlbumKey" {
		t.Errorf("albumMediaKey = %q, want AF1QipAlbumKey", amk)
	}
}

func TestDecodeCreateAlbumResponseAlbumMediaKeyMissing(t *testing.T) {
	_, err := DecodeCreateAlbumResponseAlbumMediaKey(nil)
	if err == nil {
		t.Fatal("expected error for missing album media key")
	}
}

func TestEncodeAddMediaToAlbum(t *testing.T) {
	data := EncodeAddMediaToAlbum([]string{"mk1"}, "albumKey", 1700000000, "Pixel XL", "Google", 28)
	f, err := parsePBFields(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := f.strAt(2); got != "albumKey" {
		t.Errorf("album_media_key = %q, want albumKey", got)
	}
	entries := f[1]
	if len(entries) != 1 || string(entries[0].bytes) != "mk1" {
		t.Errorf("media_keys = %v, want [mk1]", entries)
	}
	if got := f.varintAt(7); got != 1700000000 {
		t.Errorf("timestamp = %d, want 1700000000", got)
	}
}

func TestParsePBFieldsInvalid(t *testing.T) {
	_, err := parsePBFields([]byte{0xff})
	if err == nil {
		t.Fatal("expected error for truncated/invalid input")
	}
}
