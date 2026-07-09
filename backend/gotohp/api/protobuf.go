// Package api implements the unofficial Google Photos "native" (Android
// app) upload protocol against photos.googleapis.com / photosdata-pa.googleapis.com.
//
// This is a from-scratch reimplementation based on the protocol documented
// by https://github.com/xob0t/gotohp (MIT licensed) — specifically its
// .proto field definitions at https://github.com/xob0t/gotohp/tree/main/.proto.
// We do not import gotohp's generated code (its module isn't go-gettable
// and its API is built around global mutable state); instead we hand-encode
// the small subset of fields gotohp's own code actually populates. Most of
// the upstream .proto schema (particularly CommitUpload/CommitUploadResponse)
// is unpopulated placeholder structure left over from generic protobuf
// reverse-engineering and is intentionally not reproduced here.
package api

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// pbBuilder is a minimal append-only protobuf wire encoder covering just
// the varint, bytes/string, and nested-message field types used by the
// messages below.
type pbBuilder []byte

func (b pbBuilder) varint(num protowire.Number, v uint64) pbBuilder {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func (b pbBuilder) bytes(num protowire.Number, v []byte) pbBuilder {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func (b pbBuilder) str(num protowire.Number, v string) pbBuilder {
	return b.bytes(num, []byte(v))
}

// message appends nested as a length-delimited embedded message.
func (b pbBuilder) message(num protowire.Number, nested pbBuilder) pbBuilder {
	return b.bytes(num, nested)
}

// pbValue is one decoded occurrence of a field.
type pbValue struct {
	varint uint64
	bytes  []byte
}

// pbFields is a parsed protobuf message: field number -> occurrences in
// wire order. Reading methods return the last occurrence, matching proto3
// "last one wins" semantics for non-repeated fields.
type pbFields map[protowire.Number][]pbValue

func parsePBFields(b []byte) (pbFields, error) {
	fields := pbFields{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("gotohp: invalid protobuf tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf varint: %w", protowire.ParseError(n))
			}
			fields[num] = append(fields[num], pbValue{varint: v})
			b = b[n:]
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf bytes: %w", protowire.ParseError(n))
			}
			fields[num] = append(fields[num], pbValue{bytes: v})
			b = b[n:]
		case protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf fixed32: %w", protowire.ParseError(n))
			}
			fields[num] = append(fields[num], pbValue{varint: uint64(v)})
			b = b[n:]
		case protowire.Fixed64Type:
			v, n := protowire.ConsumeFixed64(b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf fixed64: %w", protowire.ParseError(n))
			}
			fields[num] = append(fields[num], pbValue{varint: v})
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf field: %w", protowire.ParseError(n))
			}
			b = b[n:]
		}
	}
	return fields, nil
}

func (f pbFields) bytesAt(num protowire.Number) []byte {
	if vs, ok := f[num]; ok && len(vs) > 0 {
		return vs[len(vs)-1].bytes
	}
	return nil
}

func (f pbFields) strAt(num protowire.Number) string {
	return string(f.bytesAt(num))
}

func (f pbFields) varintAt(num protowire.Number) uint64 {
	if vs, ok := f[num]; ok && len(vs) > 0 {
		return vs[len(vs)-1].varint
	}
	return 0
}

// msg parses field num as a nested message. Safe to chain on a nil/missing
// result since pbFields is a map type and reads on a nil map are no-ops.
func (f pbFields) msg(num protowire.Number) pbFields {
	b := f.bytesAt(num)
	if b == nil {
		return nil
	}
	nested, err := parsePBFields(b)
	if err != nil {
		return nil
	}
	return nested
}

// --- GetUploadToken (.proto/GetUploadToken.proto) ---
// int32 f1=1, f2=2, f3=3, f4=4; int64 file_size_bytes=7.
// gotohp always sends the constant f1=2,f2=2,f3=1,f4=3.
func EncodeGetUploadToken(fileSize int64) []byte {
	var b pbBuilder
	b = b.varint(1, 2)
	b = b.varint(2, 2)
	b = b.varint(3, 1)
	b = b.varint(4, 3)
	b = b.varint(7, uint64(fileSize))
	return b
}

// --- HashCheck (.proto/HashCheck.proto) ---
// field1{ field1{ bytes sha1Hash=1 }=1, field2{}=2 }
func EncodeHashCheck(sha1Hash []byte) []byte {
	var inner pbBuilder
	inner = inner.bytes(1, sha1Hash)
	var f1 pbBuilder
	f1 = f1.message(1, inner)
	f1 = f1.message(2, nil)
	var b pbBuilder
	b = b.message(1, f1)
	return b
}

// --- RemoteMatches (.proto/RemoteMatches.proto) ---
// media key lives at field1.field2.field2.media_key (string, tag 1).
func DecodeRemoteMatchesMediaKey(data []byte) (string, error) {
	f, err := parsePBFields(data)
	if err != nil {
		return "", err
	}
	return f.msg(1).msg(2).msg(2).strAt(1), nil
}

// --- CommitToken (.proto/CommitToken.proto) ---
// int64 field1=1; bytes field2=2. Returned as the raw body of the
// interactive-upload PUT response, to be echoed back into CommitUpload.
type CommitToken struct {
	Field1 uint64
	Field2 []byte
}

func DecodeCommitToken(data []byte) (CommitToken, error) {
	f, err := parsePBFields(data)
	if err != nil {
		return CommitToken{}, err
	}
	return CommitToken{Field1: f.varintAt(1), Field2: f.bytesAt(2)}, nil
}

// --- CommitUpload (.proto/CommitUpload.proto) ---
// Only the leaf fields gotohp's own code sets are encoded; the rest of the
// upstream schema's enormous nested-empty-message tree is unused dead
// structure and intentionally omitted:
//
//	field1 (msg) {
//	  field1 (msg) { field1=CommitToken.Field1, field2=CommitToken.Field2 }
//	  file_name=2 (string), sha1_hash=3 (bytes)
//	  field4 (msg) { file_last_modified_timestamp=1, field2=2 (constant 46000000) }
//	  quality=7, field10=10 (constant 1)
//	}
//	field2 (msg) { model=3, make=4, android_api_version=5 }
//	field3 = bytes{1,3}
func EncodeCommitUpload(token CommitToken, fileName string, sha1Hash []byte, uploadTimestamp int64, quality int64, model, deviceMake string, androidAPIVersion int64) []byte {
	var tokenMsg pbBuilder
	tokenMsg = tokenMsg.varint(1, token.Field1)
	tokenMsg = tokenMsg.bytes(2, token.Field2)

	var mtimeMsg pbBuilder
	mtimeMsg = mtimeMsg.varint(1, uint64(uploadTimestamp))
	mtimeMsg = mtimeMsg.varint(2, 46000000)

	var f1 pbBuilder
	f1 = f1.message(1, tokenMsg)
	f1 = f1.str(2, fileName)
	f1 = f1.bytes(3, sha1Hash)
	f1 = f1.message(4, mtimeMsg)
	f1 = f1.varint(7, uint64(quality))
	f1 = f1.varint(10, 1)

	var device pbBuilder
	device = device.str(3, model)
	device = device.str(4, deviceMake)
	device = device.varint(5, uint64(androidAPIVersion))

	var b pbBuilder
	b = b.message(1, f1)
	b = b.message(2, device)
	b = b.bytes(3, []byte{1, 3})
	return b
}

// --- CommitUploadResponse (.proto/CommitUploadResponse.proto) ---
// media key lives at field1.field3.media_key (string, tag 1).
func DecodeCommitUploadResponseMediaKey(data []byte) (string, error) {
	f, err := parsePBFields(data)
	if err != nil {
		return "", err
	}
	mk := f.msg(1).msg(3).strAt(1)
	if mk == "" {
		return "", fmt.Errorf("gotohp: commit response missing media key")
	}
	return mk, nil
}

// --- CreateAlbum (.proto/CreateAlbum.proto) ---
//
//	album_name=1, timestamp=2, field3=3 (constant 1)
//	media_keys=4 (repeated msg { field1 (msg) { media_key=1 } })
//	field6=6 (empty msg), field7=7 (msg { field1=1 (constant 3) })
//	device_info=8 (msg { model=3, make=4, android_api_version=5 })
func EncodeCreateAlbum(albumName string, mediaKeys []string, timestamp int64, model, deviceMake string, androidAPIVersion int64) []byte {
	var b pbBuilder
	b = b.str(1, albumName)
	b = b.varint(2, uint64(timestamp))
	b = b.varint(3, 1)
	for _, mk := range mediaKeys {
		var inner pbBuilder
		inner = inner.str(1, mk)
		var entry pbBuilder
		entry = entry.message(1, inner)
		b = b.message(4, entry)
	}
	b = b.message(6, nil)
	var f7 pbBuilder
	f7 = f7.varint(1, 3)
	b = b.message(7, f7)
	var device pbBuilder
	device = device.str(3, model)
	device = device.str(4, deviceMake)
	device = device.varint(5, uint64(androidAPIVersion))
	b = b.message(8, device)
	return b
}

// --- CreateAlbumResponse (.proto/CreateAlbumResponse.proto) ---
// album media key lives at field1.album_media_key (string, tag 1).
func DecodeCreateAlbumResponseAlbumMediaKey(data []byte) (string, error) {
	f, err := parsePBFields(data)
	if err != nil {
		return "", err
	}
	amk := f.msg(1).strAt(1)
	if amk == "" {
		return "", fmt.Errorf("gotohp: create album response missing album media key")
	}
	return amk, nil
}

// --- AddMediaToAlbum (.proto/AddMediaToAlbum.proto) ---
//
//	media_keys=1 (repeated string), album_media_key=2
//	field5=5 (msg { field1=1 (constant 2) })
//	device_info=6 (msg { model=3, make=4, android_api_version=5 })
//	timestamp=7
func EncodeAddMediaToAlbum(mediaKeys []string, albumMediaKey string, timestamp int64, model, deviceMake string, androidAPIVersion int64) []byte {
	var b pbBuilder
	for _, mk := range mediaKeys {
		b = b.str(1, mk)
	}
	b = b.str(2, albumMediaKey)
	var f5 pbBuilder
	f5 = f5.varint(1, 2)
	b = b.message(5, f5)
	var device pbBuilder
	device = device.str(3, model)
	device = device.str(4, deviceMake)
	device = device.varint(5, uint64(androidAPIVersion))
	b = b.message(6, device)
	b = b.varint(7, uint64(timestamp))
	return b
}
