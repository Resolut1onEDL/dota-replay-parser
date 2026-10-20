package vault

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Container names of a replay as served: bzip2, zstd (Valve switched to zstd
// under the same .bz2 URL in 2026-07) or a raw .dem.
const (
	ContainerBz2 = "bz2"
	ContainerZst = "zst"
	ContainerRaw = "raw"
)

// SniffContainer looks at the first bytes; the URL is never trusted.
func SniffContainer(head []byte) string {
	switch {
	case len(head) >= 3 && string(head[:3]) == "BZh":
		return ContainerBz2
	case len(head) >= 4 && head[0] == 0x28 && head[1] == 0xB5 && head[2] == 0x2F && head[3] == 0xFD:
		return ContainerZst
	}
	return ContainerRaw
}

// Decompress wraps r in the decoder its first bytes ask for. The returned
// close func must be called when done.
func Decompress(r io.Reader) (io.Reader, string, func(), error) {
	br := bufio.NewReaderSize(r, 4<<20)
	head, err := br.Peek(4)
	if err != nil && len(head) == 0 {
		return nil, "", nil, errors.New("empty body")
	}
	c := SniffContainer(head)
	switch c {
	case ContainerBz2:
		return bzip2.NewReader(br), c, func() {}, nil
	case ContainerZst:
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, "", nil, fmt.Errorf("zstd: %w", err)
		}
		return zr.IOReadCloser(), c, zr.Close, nil
	}
	return br, c, func() {}, nil
}

// Verify decompresses the whole stream and checks it is a Dota demo
// ("PBDEMS2" magic). It proves a download is clean before it is stored.
func Verify(r io.Reader) (container string, demBytes int64, err error) {
	dec, c, closeFn, err := Decompress(r)
	if err != nil {
		return "", 0, err
	}
	defer closeFn()
	magic := make([]byte, 7)
	if _, err := io.ReadFull(dec, magic); err != nil {
		return c, 0, fmt.Errorf("short replay: %w", err)
	}
	if !bytes.Equal(magic, []byte("PBDEMS2")) {
		return c, 0, fmt.Errorf("unexpected magic %q: expected PBDEMS2", magic)
	}
	n, err := io.Copy(io.Discard, dec)
	if err != nil {
		return c, 0, err
	}
	return c, n + 7, nil
}
