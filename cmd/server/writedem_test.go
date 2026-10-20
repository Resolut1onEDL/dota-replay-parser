package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
)

// Регрессия 2026-07-29: Valve сменил контейнер реплеев с bzip2 на zstd, не
// меняя URL (.dem.bz2). Сервис нюхал только "BZh" и писал zstd-байты прямо в
// .dem — каждый НОВЫЙ матч умирал с «unexpected magic: expected PBDEMS2», а
// разборы молча уезжали на статистику OpenDota. Старые матчи по-прежнему
// отдаются в bzip2, поэтому обязаны работать ОБА контейнера + сырой .dem.
const want = "PBDEMS2\x00hello-demo-body"

func writeDemBytes(t *testing.T, body []byte) string {
	t.Helper()
	path, n, err := writeDem(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("writeDem: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp: %v", err)
	}
	if string(got) != want {
		t.Fatalf("распаковано %q, ожидалось %q", got, want)
	}
	if int(n) != len(want) {
		t.Fatalf("записано %d байт, ожидалось %d", n, len(want))
	}
	return path
}

func TestWriteDemZstd(t *testing.T) {
	body, err := hex.DecodeString("28b52ffd2417b90000504244454d53320068656c6c6f2d64656d6f2d626f64794c58d1ad")
	if err != nil {
		t.Fatal(err)
	}
	writeDemBytes(t, body)
}

func TestWriteDemBzip2(t *testing.T) {
	body, err := hex.DecodeString("425a68393141592653598ef688f70000045f8040000002100016024800164680202000222640c46342869a60012899b9d50ee4b31f9a3083762ee48a70a1211ded11ee")
	if err != nil {
		t.Fatal(err)
	}
	writeDemBytes(t, body)
}

func TestWriteDemRaw(t *testing.T) {
	writeDemBytes(t, []byte(want))
}

// Only a replay whose bytes do not decode is a verdict on the replay. A full
// disk or a failing source says nothing about it and must not flag it bad.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write: no space left on device") }

type failAfter struct {
	r io.Reader
}

func (f failAfter) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, errors.New("read: connection reset")
	}
	return n, err
}

func TestDecodeErrorsAreToldApartFromIOErrors(t *testing.T) {
	zst, _ := hex.DecodeString("28b52ffd2417b90000504244454d53320068656c6c6f2d64656d6f2d626f64794c58d1ad")

	// the disk fills up while a good replay is unpacked
	var track trackErr
	_, err := decodeInto(failWriter{}, bytes.NewReader([]byte(want)), &track)
	if err == nil || errors.Is(err, errDecode) {
		t.Fatalf("a full disk = %v; must not be a decode error", err)
	}

	// a truncated zstd stream and a body that only claims to be bzip2 are decode errors
	for name, body := range map[string][]byte{"truncated zstd": zst[:len(zst)-9], "fake bzip2": []byte("BZh not really bzip2")} {
		path, _, err := writeDem(bytes.NewReader(body))
		if path != "" {
			os.Remove(path)
		}
		if !errors.Is(err, errDecode) {
			t.Errorf("%s = %v; want a decode error", name, err)
		}
	}

	// the source breaking mid-stream is not a verdict on the replay either
	path, _, err := writeDem(failAfter{bytes.NewReader(zst[:len(zst)-9])})
	if path != "" {
		os.Remove(path)
	}
	if err == nil || errors.Is(err, errDecode) {
		t.Fatalf("a failing source = %v; must not be a decode error", err)
	}
}
