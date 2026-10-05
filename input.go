package main

// Input container sniffing (claudedashboard extension, mirrors cmd/server
// writeDem): Valve serves new replays as ZSTD under the historical .dem.bz2
// URL (magic 28 B5 2F FD, since 2026-07-29), older ones as bzip2 ("BZh"),
// and a plain .dem starts with "PBDEMS2". The parser sniffs the first bytes
// and decompresses on the fly, so the pipeline never needs bunzip2/zstd
// binaries and never trusts the file extension.

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

func openDemo(path string) (io.Reader, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, func() {}, err
	}
	head := make([]byte, 4)
	n, rerr := io.ReadFull(f, head)
	if rerr != nil && n == 0 {
		f.Close()
		return nil, func() {}, rerr
	}
	// 4 MB buffer: both decompressors read the source in small blocks.
	full := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(head[:n]), f), 4<<20)
	closer := func() { f.Close() }
	switch {
	case n >= 3 && string(head[:3]) == "BZh":
		return bzip2.NewReader(full), closer, nil
	case n >= 4 && head[0] == 0x28 && head[1] == 0xB5 && head[2] == 0x2F && head[3] == 0xFD:
		zr, zerr := zstd.NewReader(full)
		if zerr != nil {
			f.Close()
			return nil, func() {}, zerr
		}
		return zr, func() { zr.Close(); f.Close() }, nil
	default:
		return full, closer, nil
	}
}
