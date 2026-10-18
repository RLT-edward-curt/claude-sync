package sync

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/rules"
	"github.com/tawanorg/claude-sync/internal/storage"
)

const testPass = "correct horse battery staple"

// newBucket starts an in-process S3 server with an empty bucket. No real
// storage and no real credentials are ever involved.
func newBucket(t *testing.T) storage.Storage {
	t.Helper()
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	ts := httptest.NewServer(faker.Server())
	t.Cleanup(ts.Close)
	if err := backend.CreateBucket("test"); err != nil {
		t.Fatal(err)
	}
	st, err := storage.New(&storage.StorageConfig{
		Provider: storage.ProviderS3, Bucket: "test", Region: "us-east-1",
		AccessKeyID: "test", SecretAccessKey: "test", Endpoint: ts.URL, UsePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var keyCache = map[string]string{}

func keyFile(t *testing.T, pass string) string {
	t.Helper()
	if p, ok := keyCache[pass]; ok {
		return p
	}
	dir, err := os.MkdirTemp("", "cs-key")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "age-key.txt")
	if err := crypto.GenerateKeyFromPassphrase(p, pass); err != nil {
		t.Fatal(err)
	}
	keyCache[pass] = p
	return p
}

type machine struct {
	t     *testing.T
	s     *Syncer
	dir   string
	state string
	home  string
	win   bool
	store storage.Storage
	key   string
}

func newMachine(t *testing.T, store storage.Storage, home string, windows bool) *machine {
	t.Helper()
	m := &machine{t: t, dir: t.TempDir(), state: t.TempDir(), home: home, win: windows, store: store, key: keyFile(t, testPass)}
	m.reload()
	return m
}

// reload simulates a new process: state is re-read from disk.
func (m *machine) reload() {
	st, err := LoadStateFromDir(m.state)
	if err != nil {
		m.t.Fatal(err)
	}
	enc, err := crypto.NewEncryptor(m.key)
	if err != nil {
		m.t.Fatal(err)
	}
	m.s = NewSyncerWith(&config.Config{}, m.store, enc, st, m.dir, m.home, m.win, true)
	m.s.device = "test-" + filepath.Base(m.dir)
}

func (m *machine) write(rel, content string) {
	m.t.Helper()
	p := filepath.Join(m.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		m.t.Fatal(err)
	}
	// Make mtime-based hash reuse see the change even within one second.
	future := time.Now().Add(time.Duration(len(content)%7+1) * time.Millisecond)
	_ = os.Chtimes(p, future, future)
}

func (m *machine) read(rel string) string {
	m.t.Helper()
	b, err := os.ReadFile(filepath.Join(m.dir, filepath.FromSlash(rel)))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (m *machine) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(m.dir, filepath.FromSlash(rel)))
	return err == nil
}

func (m *machine) remove(rel string) {
	if err := os.Remove(filepath.Join(m.dir, filepath.FromSlash(rel))); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) sync(opts ...Options) *Result {
	m.t.Helper()
	o := Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	res, err := m.s.Sync(context.Background(), o)
	if err != nil {
		m.t.Fatalf("sync: %v", err)
	}
	if len(res.Errors) > 0 {
		m.t.Fatalf("sync errors: %v", res.Errors)
	}
	m.reload()
	return res
}

func (m *machine) plan() *Plan {
	m.t.Helper()
	p, err := m.s.BuildPlan(context.Background())
	if err != nil {
		m.t.Fatal(err)
	}
	return p
}

func remoteKeys(t *testing.T, st storage.Storage) map[string]bool {
	objs, err := st.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, o := range objs {
		out[o.Key] = true
	}
	return out
}

func TestTwoMachinesRoundTrip(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	c := newMachine(t, b, "/Users/edwardcurt", false)

	a.write("CLAUDE.md", "hello")
	a.write("skills/x/SKILL.md", "skill")
	a.write("plugins/cache/big.bin", "not synced")
	a.write("skills/x/node_modules/y.js", "not synced")
	a.write("skills/x/.git/HEAD", "not synced")
	a.write("skills/synced/z/SKILL.md", "not synced")
	res := a.sync()
	if len(res.Uploaded) != 2 {
		t.Fatalf("uploaded %v", res.Uploaded)
	}

	res = c.sync()
	if len(res.Downloaded) != 2 || c.read("CLAUDE.md") != "hello" || c.read("skills/x/SKILL.md") != "skill" {
		t.Fatalf("downloaded %v", res.Downloaded)
	}
	if c.exists("plugins/cache/big.bin") || c.exists("skills/x/.git/HEAD") {
		t.Fatal("excluded files arrived")
	}

	// Nothing to do on either side now.
	for _, m := range []*machine{a, c} {
		p := m.plan()
		for _, act := range []Action{ActUpload, ActDownload, ActDeleteRemote, ActConflict, ActVerify} {
			if n := p.Count(act); n != 0 {
				t.Fatalf("steady state has %d %s: %v", n, act, p.Paths(act))
			}
		}
	}

	c.write("CLAUDE.md", "hello from c")
	c.sync()
	a.sync()
	if a.read("CLAUDE.md") != "hello from c" {
		t.Fatal("edit did not propagate")
	}
}

func TestExecBitOnDownloadedHook(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	c := newMachine(t, b, "/Users/edwardcurt", false)
	a.write("hooks/pre.sh", "#!/bin/sh\n")
	a.sync()
	c.sync()
	fi, err := os.Stat(filepath.Join(c.dir, "hooks", "pre.sh"))
	if err != nil || fi.Mode().Perm()&0100 == 0 {
		t.Fatalf("hook not executable: %v %v", fi.Mode(), err)
	}
}

func TestKeepBothConflict(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	c := newMachine(t, b, "/Users/edwardcurt", false)
	a.write("settings.json", `{"v":0}`)
	a.sync()
	c.sync()

	a.write("settings.json", `{"v":"a"}`)
	c.write("settings.json", `{"v":"c"}`)
	a.sync()
	res := c.sync()
	if len(res.KeptBoth) != 1 {
		t.Fatalf("expected keep-both, got %+v", res)
	}
	copyPath := res.Copies["settings.json"]
	if c.read("settings.json") != `{"v":"c"}` || c.read(copyPath) != `{"v":"a"}` {
		t.Fatal("conflict did not keep both versions")
	}
	// Copies never sync.
	if remoteKeys(t, b)[copyPath+".age"] {
		t.Fatal("conflict copy was uploaded")
	}
	// Frozen while the copy exists: nothing moves.
	res = c.sync()
	if len(res.Frozen) != 1 || len(res.Uploaded) != 0 {
		t.Fatalf("expected frozen, got %+v", res)
	}
	if a.sync(); a.read("settings.json") != `{"v":"a"}` {
		t.Fatal("a was overwritten while c was frozen")
	}
	// User merges and deletes the copy: local wins and is uploaded.
	c.write("settings.json", `{"v":"merged"}`)
	c.remove(copyPath)
	res = c.sync()
	if len(res.Uploaded) != 1 {
		t.Fatalf("merged file not uploaded: %+v", res)
	}
	a.sync()
	if a.read("settings.json") != `{"v":"merged"}` {
		t.Fatal("merge did not reach a")
	}
}

func TestTranscriptPrefixAndHistoryUnion(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	c := newMachine(t, b, "/Users/edwardcurt", false)
	tr := "projects/-Users-edwardcurt-Code-app/s1.jsonl"
	a.write(tr, "{\"n\":1}\n")
	a.write("history.jsonl", "{\"display\":\"a1\",\"timestamp\":1,\"sessionId\":\"s\"}\n")
	a.sync()
	c.sync()

	// a appends (remote grows), c appends more (local grows) -> both changed,
	// but c's copy extends a's only if c has a's lines; here c extends the
	// common base and a extends it too, so simulate the usual case: c resumed
	// the session after a's lines arrived.
	a.write(tr, "{\"n\":1}\n{\"n\":2}\n")
	a.write("history.jsonl", "{\"display\":\"a1\",\"timestamp\":1,\"sessionId\":\"s\"}\n{\"display\":\"a2\",\"timestamp\":2,\"sessionId\":\"s\"}\n")
	c.write(tr, "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n")
	c.write("history.jsonl", "{\"display\":\"a1\",\"timestamp\":1,\"sessionId\":\"s\"}\n{\"display\":\"c2\",\"timestamp\":3,\"sessionId\":\"s\"}\n")
	a.sync()
	res := c.sync()
	if len(res.KeptBoth) != 0 {
		t.Fatalf("transcript/history should auto-merge: %+v", res)
	}
	if !strings.HasSuffix(c.read(tr), "{\"n\":3}\n") {
		t.Fatal("longer transcript lost")
	}
	h := c.read("history.jsonl")
	if !strings.Contains(h, "a2") || !strings.Contains(h, "c2") {
		t.Fatalf("history union lost entries: %s", h)
	}
	a.sync()
	if a.read(tr) != c.read(tr) || !strings.Contains(a.read("history.jsonl"), "c2") {
		t.Fatal("merge did not reach a")
	}
}

func TestDeleteTombstoneNoResurrect(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	c := newMachine(t, b, "/Users/edwardcurt", false)
	a.write("memory/old.md", "x")
	a.write("memory/keep.md", "y")
	a.sync()
	c.sync()

	a.remove("memory/old.md")
	res := a.sync()
	if len(res.Deleted) != 1 {
		t.Fatalf("delete not propagated: %+v", res)
	}
	keys := remoteKeys(t, b)
	if keys["memory/old.md.age"] {
		t.Fatal("remote object still there")
	}
	foundTrash := false
	for k := range keys {
		if strings.HasPrefix(k, TrashPrefix) && strings.HasSuffix(k, "memory/old.md.age") {
			foundTrash = true
		}
	}
	if !foundTrash || !keys[TombstonesKey] {
		t.Fatal("no trash copy or tombstone")
	}

	// c still has it locally: kept, not re-uploaded, never deleted locally.
	res = c.sync()
	if !c.exists("memory/old.md") || len(res.Uploaded) != 0 || len(res.Tombstoned) != 1 {
		t.Fatalf("tombstone handling wrong: %+v", res)
	}
	if remoteKeys(t, b)["memory/old.md.age"] {
		t.Fatal("resurrected")
	}
	// Editing it on c re-shares it deliberately.
	time.Sleep(10 * time.Millisecond)
	c.write("memory/old.md", "edited")
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(filepath.Join(c.dir, "memory", "old.md"), future, future)
	res = c.sync()
	if len(res.Uploaded) != 1 {
		t.Fatalf("edited tombstoned file not re-shared: %+v", res)
	}
}

func TestOutOfScopeNeverDeletesRemote(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	// Simulate v1 having uploaded plugins/ and tracked them in state.
	enc, _ := crypto.NewEncryptor(a.key)
	for i := 0; i < 50; i++ {
		data, _ := enc.Encrypt([]byte("x"))
		key := "plugins/cache/f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".age"
		if err := b.Upload(context.Background(), key, data); err != nil {
			t.Fatal(err)
		}
		a.s.state.Files[strings.TrimSuffix(key, ".age")] = &FileState{Hash: "h", Uploaded: time.Now()}
	}
	res := a.sync()
	if len(res.Deleted) != 0 || res.Forgotten != 50 || res.OutOfScope != 50 {
		t.Fatalf("out of scope handling wrong: deleted=%d forgotten=%d outofscope=%d", len(res.Deleted), res.Forgotten, res.OutOfScope)
	}
	pp, err := a.s.BuildPrune(context.Background())
	if err != nil || len(pp.OutOfScope) != 50 {
		t.Fatalf("prune should list them: %v %v", pp, err)
	}
}

func TestMassDeletionGuard(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	for i := 0; i < 30; i++ {
		a.write("plans/p"+string(rune('a'+i%26))+string(rune('a'+i/26))+".md", "x")
	}
	a.sync()
	for i := 0; i < 30; i++ {
		a.remove("plans/p" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".md")
	}
	res := a.sync()
	if res.HeldDeletes != 30 || len(res.Deleted) != 0 {
		t.Fatalf("guard did not hold: %+v", res)
	}
	if len(remoteKeys(t, b)) < 30 {
		t.Fatal("objects deleted despite guard")
	}
	// Wrong count still holds; exact count proceeds.
	if res = a.sync(Options{ConfirmDeletes: 29}); res.HeldDeletes != 30 {
		t.Fatal("wrong confirm count accepted")
	}
	if res = a.sync(Options{ConfirmDeletes: 30}); len(res.Deleted) != 30 {
		t.Fatalf("confirmed deletes not done: %+v", res)
	}
}

func TestSelfHealsLyingState(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	a.write("agents/x.md", "agent")
	a.sync()
	// The bucket loses the object while state still says it was uploaded
	// (the v1 failure: state recorded uploads that never landed).
	if err := b.Delete(context.Background(), "agents/x.md.age"); err != nil {
		t.Fatal(err)
	}
	res := a.sync()
	if len(res.Uploaded) != 1 || !remoteKeys(t, b)["agents/x.md.age"] {
		t.Fatalf("did not self-heal: %+v", res)
	}
}

func TestV1BaselineAndFirstContact(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	a.write("rules/r.md", "rule")
	a.sync()

	// Fresh machine that already has the identical file (e.g. copied by hand):
	// verify by content, no transfer, no conflict.
	c := newMachine(t, b, "/Users/edwardcurt", false)
	c.write("rules/r.md", "rule")
	res := c.sync()
	if len(res.Uploaded)+len(res.Downloaded)+len(res.KeptBoth) != 0 || res.Recorded != 1 {
		t.Fatalf("first contact wrong: %+v", res)
	}

	// v1-style baseline (no ETag, uploaded after the remote write): recorded,
	// not transferred.
	d := newMachine(t, b, "/Users/edwardcurt", false)
	d.write("rules/r.md", "rule")
	h, _ := HashFile(filepath.Join(d.dir, "rules", "r.md"))
	d.s.state.Files["rules/r.md"] = &FileState{Path: "rules/r.md", Hash: h, Uploaded: time.Now().Add(time.Hour)}
	if err := d.s.state.Save(); err != nil {
		t.Fatal(err)
	}
	d.reload()
	p := d.plan()
	if p.Count(ActRecord) != 1 || p.Count(ActDownload)+p.Count(ActUpload) != 0 {
		t.Fatalf("v1 baseline not honored: %v", p.Items)
	}
	d.sync()
	if d.s.state.GetFile("rules/r.md").RemoteETag == "" {
		t.Fatal("ETag not seeded")
	}

	// First contact with different content: keep both, never mtime-wins.
	e := newMachine(t, b, "/Users/edwardcurt", false)
	e.write("rules/r.md", "different")
	res = e.sync()
	if len(res.KeptBoth) != 1 || e.read("rules/r.md") != "different" {
		t.Fatalf("first contact difference must keep both: %+v", res)
	}
}

func TestTooLarge(t *testing.T) {
	old := rules.MaxUploadBytes
	rules.MaxUploadBytes = 64 * 1024
	defer func() { rules.MaxUploadBytes = old }()
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	big := make([]byte, 200*1024)
	_, _ = rand.Read(big)
	a.write("projects/-Users-edwardcurt-Code-x/big.jsonl", string(big))
	a.write("projects/-Users-edwardcurt-Code-x/small.jsonl", "{}\n")
	res := a.sync()
	if len(res.TooLarge) != 1 || len(res.Uploaded) != 1 {
		t.Fatalf("size rule wrong: %+v", res)
	}
	// Unchanged big file is not retried.
	if p := a.plan(); p.Count(ActUpload) != 0 || p.Count(ActTooLarge) != 1 {
		t.Fatalf("big file retried: %v", p.Paths(ActUpload))
	}
}

func TestWrongPassphraseRefused(t *testing.T) {
	b := newBucket(t)
	a := newMachine(t, b, "/Users/edwardcurt", false)
	a.write("CLAUDE.md", "x")
	a.sync()
	wrong, _ := crypto.NewEncryptor(keyFile(t, "a different passphrase!!"))
	st, _ := LoadStateFromDir(t.TempDir())
	s := NewSyncerWith(&config.Config{}, b, wrong, st, t.TempDir(), "/Users/edwardcurt", false, true)
	if _, err := s.VerifyKey(context.Background()); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	if _, err := a.s.VerifyKey(context.Background()); err != nil {
		t.Fatalf("right passphrase rejected: %v", err)
	}
}

// Mac and Windows share a session under ~/Code: keys map to the same remote
// object, paths are rewritten to each OS's native form, JSON stays valid.
func TestCrossOSCodeSessions(t *testing.T) {
	b := newBucket(t)
	mac := newMachine(t, b, "/Users/edwardcurt", false)
	win := newMachine(t, b, `C:\Users\edward.curt`, true)

	macLine := `{"cwd":"/Users/edwardcurt/Code/app","file":"/Users/edwardcurt/Code/app/src/a.ts","other":"/Users/edwardcurt/Library/x","text":"line\nbreak"}` + "\n"
	mac.write("projects/-Users-edwardcurt-Code-app/s.jsonl", macLine)
	mac.write("projects/-Users-edwardcurt-Code-app/notes.md", "see /Users/edwardcurt/Code/app/README.md\n")
	mac.sync()
	if !remoteKeys(t, b)["projects/${HOME}-Code-app/s.jsonl.age"] {
		t.Fatalf("unexpected keys: %v", remoteKeys(t, b))
	}

	win.sync()
	wt := "projects/C--Users-edward-curt-Code-app/s.jsonl"
	got := win.read(wt)
	var obj map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &obj); err != nil {
		t.Fatalf("Windows copy is not valid JSON: %v\n%s", err, got)
	}
	if obj["cwd"] != `C:\Users\edward.curt\Code\app` || obj["file"] != `C:\Users\edward.curt\Code\app\src\a.ts` {
		t.Fatalf("paths not native on Windows: %+v", obj)
	}
	if obj["text"] != "line\nbreak" {
		t.Fatalf("JSON escape damaged: %q", obj["text"])
	}
	if md := win.read("projects/C--Users-edward-curt-Code-app/notes.md"); md != "see C:\\Users\\edward.curt\\Code\\app\\README.md\n" {
		t.Fatalf("md not native: %q", md)
	}

	// Windows resumes the session and appends a line with native paths.
	winLine := `{"cwd":"C:\\Users\\edward.curt\\Code\\app","file":"C:\\Users\\edward.curt\\Code\\app\\src\\b.ts"}` + "\n"
	win.write(wt, got+winLine)
	res := win.sync()
	if len(res.Uploaded) != 1 {
		t.Fatalf("windows append not uploaded: %+v", res)
	}
	mac.sync()
	macGot := mac.read("projects/-Users-edwardcurt-Code-app/s.jsonl")
	if !strings.HasPrefix(macGot, macLine) {
		t.Fatalf("mac lines did not round-trip:\n%s", macGot)
	}
	if !strings.Contains(macGot, `"cwd":"/Users/edwardcurt/Code/app","file":"/Users/edwardcurt/Code/app/src/b.ts"`) {
		t.Fatalf("windows line not rewritten for mac:\n%s", macGot)
	}
}

func TestRepairWindowsV1Damage(t *testing.T) {
	b := newBucket(t)
	win := newMachine(t, b, `C:\Users\edward.curt`, true)
	// What v1 wrote on Windows: ${HOME} replaced with a raw backslash path.
	broken := `{"cwd":"C:\Users\edward.curt/Code/app","ok":"fine"}` + "\n" + `{"good":"C:\\Users\\edward.curt\\x"}` + "\n"
	rel := "projects/C--Users-edward-curt-Code-app/s.jsonl"
	win.write(rel, broken)
	files, lines, err := win.s.RepairWindowsTranscripts()
	if err != nil || files != 1 || lines != 1 {
		t.Fatalf("repair: files=%d lines=%d err=%v", files, lines, err)
	}
	for _, l := range strings.Split(strings.TrimSpace(win.read(rel)), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("still invalid: %s", l)
		}
		if l == `{"good":"C:\\Users\\edward.curt\\x"}` {
			continue
		}
		if v["cwd"] != `C:\Users\edward.curt\Code\app` {
			t.Fatalf("repaired path wrong: %v", v["cwd"])
		}
	}
}

func TestPathMapperBoundaries(t *testing.T) {
	m := NewPathMapperFor(`C:\Users\edward.curt`, true)
	in := []byte(`{"a":"C:\\Users\\edward.curt\\Codex\\y","b":"C:\\Users\\edward.curtis\\z","c":"c:/users/edward.curt/Code/q"}`)
	out := string(m.NormalizeContent("projects/x/s.jsonl", in))
	if !strings.Contains(out, `${HOME}\\Codex\\y`) || !strings.Contains(out, `C:\\Users\\edward.curtis`) || !strings.Contains(out, `${HOME}/Code/q`) {
		t.Fatalf("boundary handling wrong: %s", out)
	}
	if got := m.NormalizeRelPath("projects/C--Users-edward-curt-Code-app/s.jsonl"); got != "projects/${HOME}-Code-app/s.jsonl" {
		t.Fatalf("key: %s", got)
	}
	if got, _ := m.ResolveRelPath("projects/${HOME}-Code-app/s.jsonl"); got != "projects/C--Users-edward-curt-Code-app/s.jsonl" {
		t.Fatalf("resolve key: %s", got)
	}
	mac := NewPathMapperFor("/Users/edwardcurt", false)
	if got := string(mac.NormalizeContent("history.jsonl", []byte(`/Users/edwardcurtis /Users/edwardcurt/Code`))); got != `/Users/edwardcurtis ${HOME}/Code` {
		t.Fatalf("mac boundary: %s", got)
	}
	if !bytes.Equal(mac.NormalizeContent("skills/a.md", []byte("/Users/edwardcurt")), []byte("/Users/edwardcurt")) {
		t.Fatal("content outside projects/history must not be rewritten")
	}
}

func TestRulesScope(t *testing.T) {
	cases := map[string]bool{
		"CLAUDE.md": true, "skills/a/SKILL.md": true, "projects/x/s.jsonl": true,
		"plugins/x": false, "mcp-servers/a": false, "backups/b": false,
		"skills/a/.git/config": false, "skills/synced/a/SKILL.md": false,
		"skills/a/node_modules/m.js": false, "CLAUDE.md.conflict-remote-20260101-000000": false,
		"claude-sync-shared-config.yaml": false, "history.jsonl": true,
	}
	for p, want := range cases {
		if got := rules.InScope(p); got != want {
			t.Errorf("InScope(%q) = %v, want %v", p, got, want)
		}
	}
}
