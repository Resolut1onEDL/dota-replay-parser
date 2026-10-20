package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dota-replay-parser/internal/vault"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runCmd(t *testing.T, v *vault.Vault, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), args, &out, v)
	return out.String(), err
}

func TestUploadManifestSkipsPresentAndChecksSha(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.dem", "PBDEMS2 file a")
	b := writeFile(t, dir, "b.dem", "PBDEMS2 file b")
	csv := fmt.Sprintf("id,kind,patch,accounts,path\n101,student,60,1550818411,%s\n102,audit,60,,%s\n103,camp,,,%s\n", a, b, filepath.Join(dir, "gone.dem"))
	m := writeFile(t, dir, "m.csv", csv)
	v := vault.New(vault.NewMemStore())

	out, err := runCmd(t, v, "upload", "--manifest", m)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "uploaded=2") || !strings.Contains(out, "missing=1") {
		t.Fatalf("first run:\n%s", out)
	}
	o, ok, _ := v.Find(context.Background(), 101)
	sum := sha256.Sum256([]byte("PBDEMS2 file a"))
	if !ok || o.Meta.Kind != vault.KindStudent || o.Meta.Patch != "60" || o.Meta.Accounts[0] != "1550818411" ||
		o.Meta.SHA256 != hex.EncodeToString(sum[:]) || o.Meta.Source != "local-backfill" || o.Meta.Container != vault.ContainerRaw {
		t.Fatalf("meta = %+v", o.Meta)
	}

	// rerun: everything present is skipped, nothing re-uploaded
	out, err = runCmd(t, v, "upload", "--manifest", m)
	if err != nil || !strings.Contains(out, "present=2") || strings.Contains(out, "uploaded=") {
		t.Fatalf("rerun: %v\n%s", err, out)
	}

	// the same id with different bytes is reported, never overwritten
	os.WriteFile(a, []byte("PBDEMS2 other bytes"), 0600)
	out, err = runCmd(t, v, "upload", "--manifest", m)
	if err == nil || !strings.Contains(out, "conflict=1") {
		t.Fatalf("conflict run: %v\n%s", err, out)
	}
	rc, _, _ := v.Open(context.Background(), 101)
	got := new(bytes.Buffer)
	got.ReadFrom(rc)
	if got.String() != "PBDEMS2 file a" {
		t.Fatalf("stored bytes were replaced: %q", got.String())
	}
}

func TestUploadDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.dem", "PBDEMS2 file a")
	m := writeFile(t, dir, "m.csv", fmt.Sprintf("101,student,60,7,%s\n", a))
	v := vault.New(vault.NewMemStore())
	out, err := runCmd(t, v, "upload", "--manifest", m, "--dry-run")
	if err != nil || !strings.Contains(out, "would-upload=1") {
		t.Fatalf("%v\n%s", err, out)
	}
	if ids, _ := v.ListIDs(context.Background()); len(ids) != 0 {
		t.Fatalf("dry run stored %v", ids)
	}
}

func TestManifestRejectsUnknownKind(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "m.csv", "101,tournament,60,,/x\n")
	if _, err := runCmd(t, vault.New(vault.NewMemStore()), "upload", "--manifest", m); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}

func TestPurgeStudentDryRunByDefault(t *testing.T) {
	ctx := context.Background()
	v := vault.New(vault.NewMemStore())
	put := func(id int64, kind vault.Kind, acc ...string) {
		v.PutNew(ctx, id, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: kind, Accounts: acc})
	}
	put(1, vault.KindStudent, "111")
	put(2, vault.KindStudent, "111", "222") // shared with another student
	put(3, vault.KindStudent, "222")
	put(4, vault.KindPro, "111") // not a student game
	out, err := runCmd(t, v, "purge-student", "--account", "111")
	if err != nil || !strings.Contains(out, "delete 1") || !strings.Contains(out, "detach 2") || !strings.Contains(out, "dry run") {
		t.Fatalf("%v\n%s", err, out)
	}
	if ids, _ := v.ListIDs(ctx); len(ids) != 4 {
		t.Fatalf("dry run changed the vault: %v", ids)
	}
	if _, err := runCmd(t, v, "purge-student", "--account", "111", "--apply"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := v.Exists(ctx, vault.ReplayKey(1)); ok {
		t.Fatal("sole-owner game must be deleted")
	}
	o, ok, _ := v.Find(ctx, 2)
	if !ok || strings.Join(o.Meta.Accounts, ",") != "222" {
		t.Fatalf("shared game = %+v ok=%v", o.Meta, ok)
	}
	for _, id := range []int64{3, 4} {
		if ok, _ := v.Exists(ctx, vault.ReplayKey(id)); !ok {
			t.Fatalf("game %d must stay", id)
		}
	}
	if _, err := runCmd(t, v, "purge-student", "--account", "not-a-number"); err == nil {
		t.Fatal("bad account must be rejected")
	}
}

func TestStatusAndProbeOnTheMemoryStore(t *testing.T) {
	v := vault.New(vault.NewMemStore())
	v.PutNew(context.Background(), 1, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: vault.KindLadder})
	out, err := runCmd(t, v, "status", "--version", "4.15.1")
	if err != nil || !strings.Contains(out, `"ladder"`) || !strings.Contains(out, `"objects": 1`) {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = runCmd(t, v, "probe")
	if err != nil || strings.Contains(out, "FAIL") || strings.Count(out, "ok ") != 7 {
		t.Fatalf("%v\n%s", err, out)
	}
	if keys := v.S.(*vault.MemStore).Keys("_vault/probe/"); len(keys) != 0 {
		t.Fatalf("probe left %v", keys)
	}
}

func TestReadEnvFileOnlyTakesVaultNames(t *testing.T) {
	p := writeFile(t, t.TempDir(), ".env.local", "AWS_ACCESS_KEY_ID=other\nREPLAY_VAULT_BUCKET_NAME=\"b\"\nexport REPLAY_VAULT_AWS_ACCESS_KEY_ID=k\n# c\n")
	m := readEnvFile(p)
	if len(m) != 2 || m["REPLAY_VAULT_BUCKET_NAME"] != "b" || m["REPLAY_VAULT_AWS_ACCESS_KEY_ID"] != "k" {
		t.Fatalf("env = %v", m)
	}
	if len(readEnvFile("/nonexistent/.env")) != 0 {
		t.Fatal("missing file must be empty")
	}
}

func TestUploadNeverStoresAFileThatIsNotADemo(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "good.dem", "PBDEMS2 whole file")
	html := writeFile(t, dir, "html.dem", "<html>not a replay</html>")
	zst, _ := hex.DecodeString("28b52ffd2417b90000504244454d53320068656c6c6f2d64656d6f2d626f64794c58d1ad")
	cut := writeFile(t, dir, "cut.dem", string(zst[:len(zst)-9])) // a download that stopped early
	m := writeFile(t, dir, "m.csv", fmt.Sprintf("201,student,60,7,%s\n202,student,60,7,%s\n203,pro,60,,%s\n", good, html, cut))
	v := vault.New(vault.NewMemStore())
	out, err := runCmd(t, v, "upload", "--manifest", m, "--dry-run")
	if err == nil || !strings.Contains(out, "invalid=2") || !strings.Contains(out, "would-upload=1") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	out, err = runCmd(t, v, "upload", "--manifest", m)
	if err == nil || !strings.Contains(out, "invalid=2") || !strings.Contains(out, "uploaded=1") {
		t.Fatalf("run: %v\n%s", err, out)
	}
	ids, _ := v.ListIDs(context.Background())
	if len(ids) != 1 || ids[0] != 201 {
		t.Fatalf("stored %v; only the whole file may be stored", ids)
	}
	// the truncated file was never kept, so the real one can still go in later
	os.WriteFile(cut, zst, 0600)
	if out, err = runCmd(t, v, "upload", "--manifest", m); err == nil || !strings.Contains(out, "uploaded=1") {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	if ok, _ := v.Exists(context.Background(), vault.ReplayKey(203)); !ok {
		t.Fatal("the complete file was not stored")
	}
}

func TestUploadReplacesAnObjectMarkedCorrupt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a := writeFile(t, dir, "a.dem", "PBDEMS2 whole file")
	m := writeFile(t, dir, "m.csv", fmt.Sprintf("301,student,60,7,%s\n", a))
	v := vault.New(vault.NewMemStore())
	v.PutNew(ctx, 301, strings.NewReader("junk"), 4, vault.Meta{Kind: vault.KindCoach, SHA256: "deadbeefdeadbeef"})
	v.MarkCorrupt(ctx, 301)
	out, err := runCmd(t, v, "upload", "--manifest", m)
	if err != nil || !strings.Contains(out, "uploaded=1") {
		t.Fatalf("%v\n%s", err, out)
	}
	o, _, _ := v.Find(ctx, 301)
	if o.Info.Size != int64(len("PBDEMS2 whole file")) || o.Meta.Corrupt || o.Meta.Kind != vault.KindStudent {
		t.Fatalf("after replace = %+v size %d", o.Meta, o.Info.Size)
	}
}

func TestPurgeStudentKeepsAGameThatIsAlsoProOrLadder(t *testing.T) {
	ctx := context.Background()
	v := vault.New(vault.NewMemStore())
	// a pro game a student also asked for: the student kind absorbed the pro claim
	v.PutNew(ctx, 1, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: vault.KindPro, Patch: "60"})
	v.UpdateMeta(ctx, 1, vault.Meta{Kind: vault.KindStudent, Accounts: []string{"111"}})
	v.PutNew(ctx, 2, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: vault.KindStudent, Accounts: []string{"111"}})
	out, err := runCmd(t, v, "purge-student", "--account", "111", "--apply")
	if err != nil || !strings.Contains(out, "delete 2") || !strings.Contains(out, "detach 1 (still kept as pro)") {
		t.Fatalf("%v\n%s", err, out)
	}
	o, ok, _ := v.Find(ctx, 1)
	if !ok || o.Meta.Kind != vault.KindPro || len(o.Meta.Accounts) != 0 || len(o.Meta.Also) != 0 {
		t.Fatalf("the pro game = %+v ok=%v", o.Meta, ok)
	}
	if ok, _ := v.Exists(ctx, vault.ReplayKey(2)); ok {
		t.Fatal("the student-only game must go")
	}
}

func TestPurgeStudentDoesNotDeleteAGameAnotherStudentClaimedMeanwhile(t *testing.T) {
	ctx := context.Background()
	mem := vault.NewMemStore()
	v := vault.New(mem)
	v.PutNew(ctx, 1, strings.NewReader("PBDEMS2"), 7, vault.Meta{Kind: vault.KindStudent, Accounts: []string{"111"}})
	// the delete would go ahead on the listing alone; another student claims the game just before it
	claimed := false
	orig := v.S
	v.S = &claimOnHead{Store: orig, onSecondHead: func() {
		if !claimed {
			claimed = true
			v.UpdateMeta(ctx, 1, vault.Meta{Kind: vault.KindStudent, Accounts: []string{"222"}})
		}
	}}
	out, err := runCmd(t, v, "purge-student", "--account", "111", "--apply")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := vault.New(mem).Exists(ctx, vault.ReplayKey(1)); !ok {
		t.Fatalf("a game another student claimed was deleted:\n%s", out)
	}
}

type claimOnHead struct {
	vault.Store
	heads        int
	onSecondHead func()
}

func (c *claimOnHead) Head(ctx context.Context, key string) (vault.ObjectInfo, error) {
	if key == vault.ReplayKey(1) {
		c.heads++
		if c.heads == 2 {
			c.onSecondHead()
		}
	}
	return c.Store.Head(ctx, key)
}
