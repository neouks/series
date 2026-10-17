package traffic

import (
	"bytes"
	"log"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Bodies are stored compressed wherever they land — the inline SQLite columns
// and the content-addressed blob files. The full-text index is fed from the
// in-memory plaintext at record time, so compression never affects search, and
// every reader decodes transparently:
//   - inline columns carry an 8-byte "SERIESZ1" prefix in front of the zstd
//     frame, so legacy raw rows (no prefix) pass through unchanged;
//   - blob files use the extension as the marker: ".zst" holds a bare zstd
//     frame named by the sha256 of the PLAINTEXT, ".bin" holds raw bytes.
//
// A body that does not actually shrink is stored raw, in either location.
var zstdInlinePrefix = []byte("SERIESZ1")

var (
	zstdEncOnce sync.Once
	zstdDecOnce sync.Once
	zstdEnc     *zstd.Encoder
	zstdDec     *zstd.Decoder
)

func zstdEncoder() *zstd.Encoder {
	zstdEncOnce.Do(func() {
		enc, err := zstd.NewWriter(nil,
			zstd.WithEncoderConcurrency(1),
			zstd.WithEncoderLevel(zstd.SpeedFastest))
		if err != nil {
			log.Fatalf("[traffic] zstd encoder: %v", err)
		}
		zstdEnc = enc
	})
	return zstdEnc
}

func zstdDecoder() *zstd.Decoder {
	zstdDecOnce.Do(func() {
		dec, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			log.Fatalf("[traffic] zstd decoder: %v", err)
		}
		zstdDec = dec
	})
	return zstdDec
}

// minCompressBody is the size below which a frame plus the prefix cannot pay
// for itself, so tiny bodies stay raw.
const minCompressBody = 1024

// compressInline returns the body for an exchange_bodies column: prefixed and
// zstd-compressed when smaller, unchanged otherwise.
func compressInline(body []byte) []byte {
	if len(body) < minCompressBody {
		return body
	}
	packed := zstdEncoder().EncodeAll(body, nil)
	if len(packed)+len(zstdInlinePrefix) >= len(body) {
		return body
	}
	out := make([]byte, 0, len(zstdInlinePrefix)+len(packed))
	out = append(out, zstdInlinePrefix...)
	return append(out, packed...)
}

// decompressInline reverses compressInline. Anything without the prefix —
// legacy rows, or a body that did not compress — is returned as-is; a prefixed
// body that fails to decode falls back to raw rather than failing the read.
func decompressInline(body []byte) []byte {
	if len(body) <= len(zstdInlinePrefix) || !bytes.HasPrefix(body, zstdInlinePrefix) {
		return body
	}
	plain, err := zstdDecoder().DecodeAll(body[len(zstdInlinePrefix):], nil)
	if err != nil {
		log.Printf("[traffic] zstd 正文解压失败（按原样返回 %d 字节）：%v", len(body), err)
		return body
	}
	return plain
}

// encodeBlob returns the bytes for a blob file plus whether they are a zstd
// frame (".zst") or raw (".bin"). The hash naming the file is always the
// sha256 of the plaintext, so a compressed and a raw copy of the same body
// share one identity.
func encodeBlob(body []byte) (data []byte, compressed bool) {
	if len(body) < minCompressBody {
		return body, false
	}
	packed := zstdEncoder().EncodeAll(body, nil)
	if len(packed) >= len(body) {
		return body, false
	}
	return packed, true
}

// decodeBlob reverses encodeBlob for file bytes read by extension.
func decodeBlob(data []byte, compressed bool) ([]byte, error) {
	if !compressed {
		return data, nil
	}
	plain, err := zstdDecoder().DecodeAll(data, nil)
	if err != nil {
		return nil, err
	}
	return plain, nil
}
