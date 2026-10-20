package vault

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
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
	// an unreadable kind is kept (retention deletes coach games), not guessed as coach
	for _, in := range []map[string]string{{"Kind": "bogus"}, {}, {"kind": ""}} {
		if got := MetaFromS3(in); got.Kind != KindUnknown || got.Kind.Class() != 0 {
			t.Fatalf("%v reads as %v", in, got.Kind)
		}
	}
	if _, wrote := (Meta{Kind: KindUnknown}).ToS3()["kind"]; wrote {
		t.Fatal("an unknown kind must not be written back")
	}
	if got, _ := Merge(Meta{Kind: KindUnknown}, Meta{Kind: KindCoach}); got.Kind != KindCoach {
		t.Fatalf("a hint must classify an unknown object, got %v", got.Kind)
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

func TestMergeCoachNeverBeatsProOrLadder(t *testing.T) {
	// a pro or ladder game first stored by a hint-less call is coach; the later
	// hint must win, or retention would treat it as a deletable coach copy
	for _, k := range []Kind{KindPro, KindLadder} {
		got, changed := Merge(Meta{Kind: KindCoach}, Meta{Kind: k})
		if !changed || got.Kind != k {
			t.Errorf("coach then %s = %s (changed=%v)", k, got.Kind, changed)
		}
		// and the other way round the protected kind stays
		got, _ = Merge(Meta{Kind: k}, Meta{Kind: KindCoach})
		if got.Kind != k {
			t.Errorf("%s then coach = %s", k, got.Kind)
		}
	}
	if KindCoach.Protected() || !KindPro.Protected() || !KindLadder.Protected() || !KindStudent.Protected() {
		t.Fatal("only coach is unprotected")
	}
	// student over pro keeps the pro claim in Also
	got, _ := Merge(Meta{Kind: KindPro}, Meta{Kind: KindStudent, Accounts: []string{"7"}})
	if got.Kind != KindStudent || len(got.Also) != 1 || got.Also[0] != KindPro {
		t.Fatalf("also = %+v", got)
	}
	if m := MetaFromS3(got.ToS3()); len(m.Also) != 1 || m.Also[0] != KindPro {
		t.Fatalf("also did not round-trip: %+v", m)
	}
	if HighestKind([]Kind{KindPro, KindCamp}) != KindCamp || HighestKind(nil) != KindCoach {
		t.Fatal("HighestKind")
	}
}

func TestBadIsScopedToTheParserVersion(t *testing.T) {
	m := Meta{Kind: KindCoach, Bad: true, BadVer: "4.15.2"}
	if !m.BadFor("4.15.2") || m.BadFor("4.15.3") {
		t.Fatal("bad must hold for the version that said so only")
	}
	if m.ToS3()["bad"] != "4.15.2" {
		t.Fatalf("bad stored as %q", m.ToS3()["bad"])
	}
	back := MetaFromS3(m.ToS3())
	if !back.Bad || back.BadVer != "4.15.2" {
		t.Fatalf("round trip = %+v", back)
	}
	// a flag without a version (the first format) holds for no version: it is retried once
	legacy := MetaFromS3(map[string]string{"kind": "coach", "bad": "1"})
	if !legacy.Bad || legacy.BadFor("4.15.2") {
		t.Fatalf("legacy flag = %+v", legacy)
	}
	// a newer verdict replaces an older one; a merge never clears
	got, _ := Merge(m, Meta{Kind: KindCoach, Bad: true, BadVer: "4.15.3"})
	if got.BadVer != "4.15.3" {
		t.Fatalf("merge kept %q", got.BadVer)
	}
	got, _ = Merge(m, Meta{Kind: KindCoach})
	if !got.BadFor("4.15.2") {
		t.Fatal("a merge without a verdict must keep the flag")
	}

	ctx := context.Background()
	v := New(NewMemStore())
	v.PutNew(ctx, 1, strings.NewReader("PBDEMS2"), 7, Meta{Kind: KindCoach})
	if err := v.MarkBad(ctx, 1, "4.15.2"); err != nil {
		t.Fatal(err)
	}
	if o, _, _ := v.Find(ctx, 1); !o.Meta.BadFor("4.15.2") {
		t.Fatalf("after MarkBad = %+v", o.Meta)
	}
	if err := v.ClearBad(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if o, _, _ := v.Find(ctx, 1); o.Meta.Bad || o.Meta.BadVer != "" {
		t.Fatalf("after ClearBad = %+v", o.Meta)
	}
}

func TestUpdateMetaDoesNotOverwriteAConcurrentUpdate(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	v := New(mem)
	v.PutNew(ctx, 5, strings.NewReader("PBDEMS2"), 7, Meta{Kind: KindCoach, Accounts: []string{"1"}})
	// B lands between A's read and A's write
	fired := false
	mem.BeforeCopy = func(key string) {
		if fired {
			return
		}
		fired = true
		other := Meta{Kind: KindCoach, Accounts: []string{"1", "3"}}
		if err := mem.CopyReplace(ctx, key, other.ToS3()); err != nil {
			t.Error(err)
		}
	}
	if _, err := v.UpdateMeta(ctx, 5, Meta{Kind: KindStudent, Accounts: []string{"2"}}); err != nil {
		t.Fatal(err)
	}
	o, _, _ := v.Find(ctx, 5)
	if o.Meta.Kind != KindStudent || strings.Join(o.Meta.Accounts, ",") != "1,2,3" {
		t.Fatalf("lost update: %+v", o.Meta)
	}
}

func TestParallelMetaUpdatesAllLand(t *testing.T) {
	ctx := context.Background()
	v := New(NewMemStore())
	v.PutNew(ctx, 6, strings.NewReader("PBDEMS2"), 7, Meta{Kind: KindCoach})
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := v.UpdateMeta(ctx, 6, Meta{Kind: KindStudent, Accounts: []string{fmt.Sprint(i)}}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	o, _, _ := v.Find(ctx, 6)
	if len(o.Meta.Accounts) != 8 {
		t.Fatalf("accounts = %v", o.Meta.Accounts)
	}
}

func TestPutNewReplacesACorruptObjectOnly(t *testing.T) {
	ctx := context.Background()
	v := New(NewMemStore())
	v.PutNew(ctx, 8, strings.NewReader("old broken bytes"), 16, Meta{Kind: KindPro, Patch: "60", Accounts: []string{"4"}, Container: ContainerBz2, Bad: true, BadVer: "4.15.1"})
	// not corrupt: the bytes stay, whatever the caller offers
	if stored, err := v.PutNew(ctx, 8, strings.NewReader("PBDEMS2 new"), 11, Meta{Kind: KindCoach}); err != nil || stored {
		t.Fatalf("healthy object: stored=%v err=%v", stored, err)
	}
	if err := v.MarkCorrupt(ctx, 8); err != nil {
		t.Fatal(err)
	}
	stored, err := v.PutNew(ctx, 8, strings.NewReader("PBDEMS2 new"), 11, Meta{Kind: KindStudent, Accounts: []string{"9"}, Container: ContainerRaw})
	if err != nil || !stored {
		t.Fatalf("corrupt object: stored=%v err=%v", stored, err)
	}
	o, _, _ := v.Find(ctx, 8)
	if o.Info.Size != 11 || o.Meta.Corrupt || o.Meta.Bad || o.Meta.Container != ContainerRaw ||
		o.Meta.Kind != KindStudent || o.Meta.Patch != "60" || strings.Join(o.Meta.Accounts, ",") != "4,9" {
		t.Fatalf("replaced object = %+v size %d", o.Meta, o.Info.Size)
	}
	if len(o.Meta.Also) != 1 || o.Meta.Also[0] != KindPro {
		t.Fatalf("the pro claim was lost: %+v", o.Meta.Also)
	}
}

func TestFinalFailureIndexAndFailureCount(t *testing.T) {
	ctx := context.Background()
	v := New(NewMemStore())
	v.WriteDone(ctx, "4.15.1", 1, Done{OK: true})
	v.WriteDone(ctx, "4.15.1", 2, Done{Final: true, Error: "web_413"})
	v.WriteDone(ctx, "4.15.1", 3, Done{Final: true, Error: "web_413"})
	v.WriteDone(ctx, "4.15.1", 4, Done{Final: true, Error: "parse_failed"})
	v.WriteDone(ctx, "4.15.0", 5, Done{Final: true, Error: "web_413"})
	got, err := v.FinalFailures(ctx, "4.15.1")
	if err != nil || got["web_413"] != 2 || got["parse_failed"] != 1 || len(got) != 2 {
		t.Fatalf("final failures = %v %v", got, err)
	}
	if set, _ := v.DoneSet(ctx, "4.15.1"); len(set) != 4 {
		t.Fatalf("done set = %v", set)
	}
	for want := 1; want <= 3; want++ {
		if n, err := v.BumpFailure(ctx, "4.15.1", 9, "boom"); err != nil || n != want {
			t.Fatalf("bump %d = %d %v", want, n, err)
		}
	}
}
