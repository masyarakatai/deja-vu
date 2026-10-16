package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vshulcz/deja-vu/internal/model"
	"github.com/vshulcz/deja-vu/internal/tui"
)

// treeHash is every file under root with its bytes, so a put-back can be
// compared with what was deleted.
func treeHash(t *testing.T, roots ...string) string {
	t.Helper()
	var lines []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, _ := os.ReadFile(p)
			sum := sha256.Sum256(b)
			lines = append(lines, p+" "+hex.EncodeToString(sum[:]))
			return nil
		})
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// deja delete moves a Claude Code session, its subagent directory with it,
// out of Claude's store, and --undo puts back the same bytes.
func TestDeleteAndUndoPutBackTheSameBytes(t *testing.T) {
	dir, root := tuiStore(t)
	tr := filepath.Join(root, "payments", "b2222222-retry.jsonl")
	side := filepath.Join(root, "payments", "b2222222-retry", "subagents", "agent-1.jsonl")
	writeFile(t, side, `{"sub":"agent"}`+"\n")
	before := treeHash(t, tr, filepath.Dir(filepath.Dir(side)))

	var out bytes.Buffer
	if err := runDelete(dir, []string{"b2222222"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "deleted b2222222") || !strings.Contains(out.String(), "deja delete --undo") {
		t.Errorf("delete said %q", out.String())
	}
	for _, p := range []string{tr, filepath.Dir(filepath.Dir(side))} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s is still in Claude's store", p)
		}
	}
	if s, ok, _ := findByPrefix(dir, "b2222222"); !ok || !transcriptGone(s) {
		t.Errorf("the deleted session does not read as gone from its agent (found %v)", ok)
	}
	out.Reset()
	if err := runDelete(dir, []string{"--list"}, &out); err != nil || !strings.Contains(out.String(), "the retry double charges the card") {
		t.Errorf("--list said %q, %v", out.String(), err)
	}
	out.Reset()
	if err := runDelete(dir, []string{"--undo", "b2222222"}, &out); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, tr, filepath.Dir(filepath.Dir(side))); after != before {
		t.Errorf("put back is not what was deleted:\nbefore\n%s\nafter\n%s", before, after)
	}
	if es := trashed(dir); len(es) != 0 {
		t.Errorf("the trash still holds %d entries after undo", len(es))
	}
}

// A file or directory that also holds another indexed session is never
// moved, and neither is a session in a database deja cannot put back.
func TestDeleteRefusesWhatIsNotOneSessions(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	hist := filepath.Join(home, "history.jsonl")
	writeFile(t, hist, "{}\n")
	s := model.Session{Harness: "claude", ID: "aaaa1111-x", Path: hist}
	if _, err := trashSession(dir, s, []string{hist, hist}); err == nil || !strings.Contains(err.Error(), "holds 2 sessions") {
		t.Errorf("a shared transcript: %v", err)
	}
	// A directory named by the session that another session lives under.
	sd := filepath.Join(home, "sessions", "bbbb2222-session")
	mine, theirs := filepath.Join(sd, "events.jsonl"), filepath.Join(sd, "child", "events.jsonl")
	writeFile(t, mine, "{}\n")
	writeFile(t, theirs, "{}\n")
	s = model.Session{Harness: "copilot", ID: "bbbb2222-session", Path: mine}
	if _, err := trashSession(dir, s, []string{mine, theirs}); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Errorf("a directory with another session in it: %v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Fatal("the other session was moved")
	}
	// Directory-per-session agents move the whole directory.
	if err := os.RemoveAll(filepath.Join(sd, "child")); err != nil {
		t.Fatal(err)
	}
	e, err := trashSession(dir, s, []string{mine})
	if err != nil || len(e.Files) != 1 || e.Files[0].From != sd {
		t.Fatalf("a session directory: %+v, %v", e, err)
	}
	// A database store.
	db := filepath.Join(home, "threads.db")
	writeFile(t, db, "sqlite")
	s = model.Session{Harness: "zed", ID: "cccc3333", Path: db}
	if _, err := trashSession(dir, s, []string{db, db}); err == nil || !strings.Contains(err.Error(), "can't delete Zed") {
		t.Errorf("a database store: %v", err)
	}
	// A checked agent whose path is a database holding several sessions.
	s = model.Session{Harness: "copilot", ID: "dddd4444-x", Path: db}
	if _, err := trashSession(dir, s, []string{db, db}); err == nil || !strings.Contains(err.Error(), "database") {
		t.Errorf("a shared database under a checked agent: %v", err)
	}
	if _, err := os.Stat(db); err != nil {
		t.Error("the database was moved")
	}
}

// A running session is refused: its agent would write it again next turn.
func TestDeleteRefusesARunningSession(t *testing.T) {
	dir, root := tuiStore(t)
	markSessionLive(dir, "b2222222-retry")
	s := model.Session{Harness: "claude", ID: "b2222222-retry", Path: filepath.Join(root, "payments", "b2222222-retry.jsonl")}
	if _, err := trashSession(dir, s, nil); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("a live session: %v", err)
	}
	if _, err := os.Stat(s.Path); err != nil {
		t.Error("a live session's transcript was moved")
	}
}

// Undo never writes over a file the agent has made again since.
func TestUndoKeepsANewerFile(t *testing.T) {
	dir, root := tuiStore(t)
	tr := filepath.Join(root, "payments", "b2222222-retry.jsonl")
	if err := runDelete(dir, []string{"b2222222"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, tr, "new\n")
	if err := runDelete(dir, []string{"--undo", "b2222222"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "exists again") {
		t.Errorf("undo over a new file: %v", err)
	}
	if b, _ := os.ReadFile(tr); string(b) != "new\n" {
		t.Error("undo wrote over the newer file")
	}
	if len(trashed(dir)) != 1 {
		t.Error("the trashed copy was dropped")
	}
}

func TestTrashLetsGoAfterThirtyDays(t *testing.T) {
	dir, _ := tuiStore(t)
	if err := runDelete(dir, []string{"b2222222"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if n := emptyTrash(dir, time.Now().Add(29*24*time.Hour)); n != 0 {
		t.Errorf("emptied %d at 29 days", n)
	}
	if n := emptyTrash(dir, time.Now().Add(31*24*time.Hour)); n != 1 {
		t.Errorf("emptied %d at 31 days", n)
	}
}

// opencode keeps sessions in its database; its own export, delete and import
// are the trash and the undo.
func TestDeleteOpencodeThroughItsOwnCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in opencode is a shell script")
	}
	dir := t.TempDir()
	bin, store := t.TempDir(), t.TempDir()
	log := filepath.Join(store, "log")
	script := `#!/bin/sh
echo "$@" >> ` + log + `
case "$1" in
export) [ -f ` + store + `/$2 ] && { echo "Exporting session: $2" >&2; cat ` + store + `/$2; } ;;
session) rm ` + store + `/$3 ;;
import) id=$(sed -n 's/.*"id": *"\([^"]*\)".*/\1/p' "$2" | head -1); cp "$2" ` + store + `/$id ;;
esac
`
	writeFile(t, filepath.Join(bin, "opencode"), script)
	if err := os.Chmod(filepath.Join(bin, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	body := `{"info": {"id": "ses_abc12345"}, "messages": [1, 2, 3]}`
	writeFile(t, filepath.Join(store, "ses_abc12345"), body)
	s := model.Session{Harness: "opencode", ID: "ses_abc12345", Path: "/w/q"}
	if _, err := trashSession(dir, s, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store, "ses_abc12345")); err == nil {
		t.Fatal("opencode still has the session")
	}
	if _, err := untrashSession(dir, "opencode", "ses_abc12345"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(store, "ses_abc12345")); string(b) != body {
		t.Errorf("opencode got back %q", b)
	}
	// An export that is not the session stops the delete before it runs.
	writeFile(t, filepath.Join(store, "ses_zzz99999"), `{"info": {"id": "ses_other"}}`)
	if _, err := trashSession(dir, model.Session{Harness: "opencode", ID: "ses_zzz99999"}, nil); err == nil {
		t.Error("a wrong export went ahead")
	}
	if _, err := os.Stat(filepath.Join(store, "ses_zzz99999")); err != nil {
		t.Error("the session was deleted after a wrong export")
	}
}

// D in the list asks, enter deletes, and R on the same card puts it back.
func TestTUIDeleteAndPutBack(t *testing.T) {
	dir, root := tuiStore(t)
	a := newTestTUI(t, dir)
	tr := filepath.Join(root, "payments", "b2222222-retry.jsonl")
	before := treeHash(t, tr)
	for i, r := range a.rows {
		if r.s.ID == "b2222222-retry" {
			a.sel = i
		}
	}
	a.listFocus = true
	a.handle(tui.Event{Kind: tui.EvKey, Key: tui.KeyRune, Rune: 'D'})
	if a.modal != modalDelete {
		t.Fatalf("D opened modal %d", a.modal)
	}
	wantOnScreen(t, screen(a.frame(100, 30)), "Delete it from Claude Code?", "deja keeps it 30 days")
	a.handle(tui.Event{Kind: tui.EvKey, Key: tui.KeyEnter})
	settle(a)
	if _, err := os.Stat(tr); err == nil {
		t.Fatal("the transcript is still there")
	}
	if !a.keptIDs[sessionKey(a.rows[a.sel].s)] {
		t.Error("the deleted session is not under Deleted")
	}
	a.handle(tui.Event{Kind: tui.EvKey, Key: tui.KeyRune, Rune: 'R'})
	settle(a)
	if treeHash(t, tr) != before {
		t.Error("R did not put back the same bytes")
	}
}

// Cursor's CLI keeps the conversation in chats/<hash>/<id>; the transcript
// deja reads is a copy, and moving only it left the session in cursor-agent.
func TestDeleteTakesCursorsChatStoreToo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	id := "cf555555-f21f-4493-b8f8-7eaf2c0a80d4"
	tr := filepath.Join(home, ".cursor", "projects", "p", "agent-transcripts", id, id+".jsonl")
	store := filepath.Join(home, ".cursor", "chats", "42802a0c", id, "store.db")
	writeFile(t, tr, "{}\n")
	writeFile(t, store, "db")
	before := treeHash(t, filepath.Dir(tr), filepath.Dir(store))
	e, err := trashSession(dir, model.Session{Harness: "cursor", ID: id, Path: tr}, []string{tr})
	if err != nil || len(e.Files) != 2 {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := os.Stat(filepath.Dir(store)); err == nil {
		t.Error("cursor-agent still has the chat")
	}
	if _, err := untrashSession(dir, "cursor", id); err != nil {
		t.Fatal(err)
	}
	if treeHash(t, filepath.Dir(tr), filepath.Dir(store)) != before {
		t.Error("not put back as it was")
	}
}

// settle waits for the screen's background work, then applies what it sent:
// a delete starts a fresh scan of the deleted list besides its own update.
func settle(a *tuiApp) {
	for {
		a.bg.Wait()
		select {
		case f := <-a.updates:
			f()
		default:
			return
		}
	}
}
