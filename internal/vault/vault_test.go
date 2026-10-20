package vault

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestParseKind(t *testing.T) {
	if k, ok := ParseKind(""); !ok || k != KindCoach {
		t.Fatalf("empty = %v %v, want coach", k, ok)
	}
	if k, ok := ParseKind(" Student "); !ok || k != KindStudent {
		t.Fatalf("Student = %v %v", k, ok)
	}
	if _, ok := ParseKind("tournament"); ok {
		t.Fatal("unknown kind must be rejected")
	}
}

func TestKeyRoundTrip(t *testing.T) {
	id, ok := ParseReplayKey(ReplayKey(9048497893))
	if !ok || id != 9048497893 {
		t.Fatalf("round trip = %d %v", id, ok)
	}
	if _, ok := ParseReplayKey("replays/x.dem"); ok {
		t.Fatal("bad key parsed")
	}
	if _, ok := ParseReplayKey("_vault/done/4.15.1/1"); ok {
		t.Fatal("control key parsed as replay")
	}
}

func TestMetaRoundTrip(t *testing.T) {
	in := Meta{Kind: KindStudent, Patch: "60", Accounts: []string{"20", "3", "20", "x"}, Source: "autoparse",
		Container: ContainerZst, StoredAt: "2026-10-20T10:00:00Z", Bad: true}
	m := in.ToS3()
	if m["accounts"] != "20,3" && m["accounts"] != "3,20" {
		t.Fatalf("accounts = %q", m["accounts"])
	}
	out := MetaFromS3(m)
	if out.Kind != KindStudent || out.Patch != "60" || len(out.Accounts) != 2 || !out.Bad || out.Container != ContainerZst {
		t.Fatalf("round trip = %+v", out)
	}
	if got := MetaFromS3(map[string]string{"Kind": "bogus"}); got.Kind != KindCoach {
		t.Fatalf("unknown kind reads as %v", got.Kind)
	}
	if CleanPatch("7.41") != "" || CleanPatch("60") != "60" {
		t.Fatal("patch must be an integer id")
	}
}

func TestMergeRules(t *testing.T) {
	have := Meta{Kind: KindPro, Accounts: []string{"1"}}
	got, changed := Merge(have, Meta{Kind: KindStudent, Accounts: []string{"2"}, Patch: "60"})
	if !changed || got.Kind != KindStudent || strings.Join(got.Accounts, ",") != "1,2" || got.Patch != "60" {
		t.Fatalf("higher class must win and unions/fills: %+v", got)
	}
	// same class: the first-written kind stays
	got, _ = Merge(Meta{Kind: KindPro}, Meta{Kind: KindCoach})
	if got.Kind != KindPro {
		t.Fatalf("same class kept %v, want pro", got.Kind)
	}
	// lower class never downgrades; a known patch is not overwritten
	got, changed = Merge(Meta{Kind: KindStudent, Patch: "59"}, Meta{Kind: KindCoach, Patch: "60"})
	if changed || got.Kind != KindStudent || got.Patch != "59" {
		t.Fatalf("no change expected: %+v changed=%v", got, changed)
	}
	// bad is sticky
	got, _ = Merge(Meta{Kind: KindCoach, Bad: true}, Meta{Kind: KindCoach})
	if !got.Bad {
		t.Fatal("bad must be sticky")
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{"BUCKET_NAME": "b", "AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s", "AWS_ENDPOINT_URL_S3": "https://x"}
	c := ConfigFromEnv(func(k string) string { return env[k] })
	if !c.Enabled() || c.Region != "auto" || c.Endpoint != "https://x" {
		t.Fatalf("cfg = %+v", c)
	}
	if ConfigFromEnv(func(string) string { return "" }).Enabled() {
		t.Fatal("empty env must leave the vault off")
	}
	// local tools: only the prefixed names count
	local := ConfigFromPrefix(func(k string) string {
		return map[string]string{"REPLAY_VAULT_BUCKET_NAME": "lb", "REPLAY_VAULT_AWS_ACCESS_KEY_ID": "a",
			"REPLAY_VAULT_AWS_SECRET_ACCESS_KEY": "b", "AWS_ACCESS_KEY_ID": "other"}[k]
	}, "REPLAY_VAULT_")
	if !local.Enabled() || local.AccessKeyID != "a" || local.Bucket != "lb" {
		t.Fatalf("prefixed cfg = %+v", local)
	}
}

func TestPutNewMergesWhenPresent(t *testing.T) {
	ctx := context.Background()
	v := New(NewMemStore())
	body := []byte("PBDEMS2 bytes")
	stored, err := v.PutNew(ctx, 100, bytes.NewReader(body), int64(len(body)), Meta{Kind: KindCoach, Accounts: []string{"5"}})
	if err != nil || !stored {
		t.Fatalf("first put: %v %v", stored, err)
	}
	stored, err = v.PutNew(ctx, 100, bytes.NewReader([]byte("other")), 5, Meta{Kind: KindStudent, Accounts: []string{"6"}, Patch: "60"})
	if err != nil || stored {
		t.Fatalf("second put: stored=%v err=%v; want merge", stored, err)
	}
	o, ok, _ := v.Find(ctx, 100)
	if !ok || o.Meta.Kind != KindStudent || strings.Join(o.Meta.Accounts, ",") != "5,6" || o.Meta.Patch != "60" {
		t.Fatalf("merged = %+v", o.Meta)
	}
	rc, _, _ := v.Open(ctx, 100)
	got := new(bytes.Buffer)
	got.ReadFrom(rc)
	if got.String() != string(body) {
		t.Fatalf("bytes were overwritten: %q", got.String())
	}
}

func TestDoneSetAndValveLog(t *testing.T) {
	ctx := context.Background()
	v := New(NewMemStore())
	v.WriteDone(ctx, "4.15.1", 7, Done{OK: true, Kind: KindStudent})
	v.WriteDone(ctx, "4.15.0", 8, Done{OK: true})
	set, _ := v.DoneSet(ctx, "4.15.1")
	if len(set) != 1 || !set[7] {
		t.Fatalf("done set = %v", set)
	}
	v.LogValve(ctx, "server", "backfill", 1, 200)
	v.LogValve(ctx, "server", "parse", 2, 200)
	v.LogValve(ctx, "worker", "reparse", 3, 200)
	c, err := v.CountValve(ctx, time.Now().Add(-time.Hour))
	if err != nil || c["server"] != 2 || c["worker"] != 1 {
		t.Fatalf("valve counts = %v %v", c, err)
	}
}

func TestVerifyContainers(t *testing.T) {
	const want = "PBDEMS2\x00hello-demo-body"
	zst, _ := hex.DecodeString("28b52ffd2417b90000504244454d53320068656c6c6f2d64656d6f2d626f64794c58d1ad")
	if c, n, err := Verify(bytes.NewReader(zst)); err != nil || c != ContainerZst || n != int64(len(want)) {
		t.Fatalf("zstd: %v %d %v", c, n, err)
	}
	if c, _, err := Verify(strings.NewReader(want)); err != nil || c != ContainerRaw {
		t.Fatalf("raw: %v %v", c, err)
	}
	if _, _, err := Verify(strings.NewReader("not a demo at all")); err == nil {
		t.Fatal("garbage must not verify")
	}
	if _, _, err := Verify(bytes.NewReader(zst[:len(zst)-6])); err == nil {
		t.Fatal("truncated zstd must not verify")
	}
}
