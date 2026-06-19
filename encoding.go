// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// errUnsupportedEncoding signals a Content-Encoding we don't advertise. The
// handler turns it into a 415 (RFC 7694) so aperture downgrades to a coding we
// accept (or identity) and retries — it is never a block of the user's request.
var errUnsupportedEncoding = errors.New("unsupported content-encoding")

// reqCodec decodes one inbound Content-Encoding. The decode operates on the
// full body (already in memory after io.ReadAll), so it takes and returns
// bytes rather than wrapping a stream.
type reqCodec struct {
	name   string
	decode func(body []byte) ([]byte, error)
}

// reqCodecs lists the request-body codings we accept, best first. The order is
// the advertised preference: aperture picks the first one it can encode with.
// Both the Accept-Encoding header (acceptEncoding) and the decode dispatch
// (decodeRequestBody) derive from this single list, so what we advertise can
// never drift from what we can actually decode. Adding a coding (e.g. "br") is
// a single entry here.
//
// gzip is the universal floor (stdlib, every client has it); zstd is preferred
// for its better ratio at comparable decode cost. Decoding is cheap for both —
// the work this buys is on aperture's side, where a smaller body on the wire
// cuts the inbound transfer (read_ms) that dominates warm-call latency.
var reqCodecs = []reqCodec{
	{name: "zstd", decode: decodeZstd},
	{name: "gzip", decode: decodeGzip},
}

// acceptEncoding is the Accept-Encoding value we advertise on responses, built
// from reqCodecs in preference order (e.g. "zstd, gzip"). Per RFC 7694 this
// tells aperture which request-body codings we accept; it learns it from the
// response and compresses subsequent request bodies accordingly.
var acceptEncoding = func() string {
	names := make([]string, len(reqCodecs))
	for i, c := range reqCodecs {
		names[i] = c.name
	}
	return strings.Join(names, ", ")
}()

// sharedZstdDecoder is a single concurrency-safe decoder reused across
// requests. Created with a nil source, its DecodeAll is safe for concurrent
// use and spawns no per-call goroutines (unlike streaming zstd.NewReader).
// WithDecoderMaxMemory caps the decompressed size as a decompression-bomb
// guard, matching the maxBody ceiling on the wire read.
var sharedZstdDecoder = func() *zstd.Decoder {
	d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxBody))
	if err != nil {
		// NewReader only errors on bad options; ours are static, so this is
		// unreachable. Panic rather than ship a nil decoder.
		panic(fmt.Sprintf("init zstd decoder: %v", err))
	}
	return d
}()

// decodeRequestBody decompresses body according to a request's Content-Encoding
// header value. An empty or "identity" encoding returns body unchanged. An
// encoding we don't advertise returns errUnsupportedEncoding (handler -> 415).
// A decode failure on a supported coding returns a wrapped error (handler fails
// open). Matching is case-insensitive; any q-value is ignored (request
// Content-Encoding carries none in practice, but be lenient).
func decodeRequestBody(body []byte, encoding string) ([]byte, error) {
	enc := strings.ToLower(strings.TrimSpace(strings.SplitN(encoding, ";", 2)[0]))
	if enc == "" || enc == "identity" {
		return body, nil
	}
	for _, c := range reqCodecs {
		if c.name == enc {
			return c.decode(body)
		}
	}
	return nil, fmt.Errorf("%w: %q", errUnsupportedEncoding, enc)
}

func decodeZstd(body []byte) ([]byte, error) {
	out, err := sharedZstdDecoder.DecodeAll(body, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decode: %w", err)
	}
	return out, nil
}

func decodeGzip(body []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer zr.Close()
	// Cap the decompressed size to the same ceiling as the wire read so a
	// small bomb can't blow up memory. LimitReader leaves us one byte over the
	// cap to detect overflow.
	out, err := io.ReadAll(io.LimitReader(zr, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("gzip decode: %w", err)
	}
	if len(out) > maxBody {
		return nil, fmt.Errorf("gzip decode: decompressed body exceeds %d bytes", maxBody)
	}
	return out, nil
}
