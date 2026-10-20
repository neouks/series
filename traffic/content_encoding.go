package traffic

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// maxDecodedBody bounds one decompressed HTTP body. Captured bodies are already
// bounded by the proxy, but a malicious Content-Encoding must not be able to
// turn a small frame into an unbounded allocation just because a reader or the
// full-text index asks for the plaintext.
const maxDecodedBody = 64 << 20

// decodeHTTPBody decompresses body according to the Content-Encoding header in
// the captured raw HTTP head. It returns the original bytes unchanged when there
// is no encoding, the encoding is unsupported, or decoding fails: capture must
// never lose the raw bytes because a server sent something unexpected.
//
// Content-Encoding lists transforms in the order they were applied, so a value
// like "gzip, br" is decoded in reverse (br first, then gzip).
func decodeHTTPBody(head string, body []byte) []byte {
	encodings := contentEncodings(head)
	if len(body) == 0 || len(encodings) == 0 {
		return body
	}
	out := body
	for i := len(encodings) - 1; i >= 0; i-- {
		decoded, err := decodeOne(encodings[i], out)
		if err != nil {
			return body
		}
		out = decoded
	}
	return out
}

// contentEncodings returns the non-identity Content-Encoding values in head.
// Header names are matched case-insensitively and repeated headers are merged.
func contentEncodings(head string) []string {
	var encodings []string
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "Content-Encoding") {
			continue
		}
		for _, part := range strings.Split(value, ",") {
			enc := strings.ToLower(strings.TrimSpace(part))
			if enc != "" && enc != "identity" {
				encodings = append(encodings, enc)
			}
		}
	}
	return encodings
}

func decodeOne(encoding string, body []byte) ([]byte, error) {
	switch encoding {
	case "gzip", "x-gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return readDecoded(r)
	case "deflate":
		// HTTP "deflate" is zlib-wrapped, but some servers send raw DEFLATE.
		if r, err := zlib.NewReader(bytes.NewReader(body)); err == nil {
			defer r.Close()
			return readDecoded(r)
		}
		r := flate.NewReader(bytes.NewReader(body))
		defer r.Close()
		return readDecoded(r)
	case "br":
		return readDecoded(brotli.NewReader(bytes.NewReader(body)))
	case "zstd":
		r, err := zstd.NewReader(bytes.NewReader(body),
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(maxDecodedBody))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return readDecoded(r)
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", encoding)
	}
}

func readDecoded(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, maxDecodedBody+1))
	if err != nil {
		return nil, err
	}
	if n > maxDecodedBody {
		return nil, fmt.Errorf("decoded body exceeds %d bytes", maxDecodedBody)
	}
	return buf.Bytes(), nil
}
