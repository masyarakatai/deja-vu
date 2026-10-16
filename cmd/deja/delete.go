package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vshulcz/deja-vu/internal/digest"
	"github.com/vshulcz/deja-vu/internal/index"
	"github.com/vshulcz/deja-vu/internal/model"
	"github.com/vshulcz/deja-vu/internal/search"
	"github.com/vshulcz/deja-vu/internal/sources"
)

// `deja delete <id>` takes a session out of its agent the way deleting the
// file by hand does, except the file is moved into deja's trash rather than
// unlinked: `deja delete --undo <id>` and R in the TUI put back the very
// bytes, tool calls and all, which the write-back from the index cannot.
// The trash keeps a session for trashKeep and then lets it go; deja's own
// copy in the index stays searchable after that, as for any session its
// agent deleted.

const trashKeep = 30 * 24 * time.Hour

// trashEntry is one session in the trash and where each of its files lived.
type trashEntry struct {
	Harness string      `json:"harness"`
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	At      time.Time   `json:"at"`
	Files   []trashFile `json:"files,omitempty"`
	Export  string      `json:"export,omitempty"` // a cliStore's export, in the entry's directory
}

type trashFile struct {
	From string `json:"from"` // where the agent kept it
	Name string `json:"name"` // its name inside the entry's directory
}

func trashDir(dir string) string { return dir + ".trash" }

func trashEntryDir(dir, harness, id string) string {
	return filepath.Join(trashDir(dir), harness, id)
}

// deletableFiles is what one session is on disk, for an agent that keeps a
// session as files of its own: the transcript, or the directory named by the
// session's id that holds it (Cursor, Copilot, Cline, Grok, Kimi, Antigravity
// and dsh keep one per session), and for Claude Code the directory of
// subagent runs and tool results beside it. Anything another indexed session
// also lives in is refused: Claude's history.jsonl holds every prompt, and a
// shared database is not one session's to move. others is every indexed
// session's path.
func deletableFiles(s model.Session, others []string) ([]string, error) {
	if !deletable[s.Harness] {
		return nil, errNotDeletable
	}
	if s.Path == "" || strings.Contains(filepath.ToSlash(s.Path), "/globalStorage/") {
		// An editor extension's task: the editor keeps the list.
		return nil, errNotDeletable
	}
	fi, err := os.Lstat(s.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotFileBacked
	}
	files := []string{s.Path}
	if root := sessionDir(s); root != "" {
		files = []string{root}
	} else if side := strings.TrimSuffix(s.Path, filepath.Ext(s.Path)); filepath.Base(side) == s.ID {
		if fi, err := os.Lstat(side); err == nil && fi.IsDir() {
			files = append(files, side)
		}
	}
	files = append(files, companions(s, files)...)
	for _, f := range files {
		for _, o := range others {
			if o == s.Path {
				continue
			}
			if o == f || strings.HasPrefix(o, f+string(filepath.Separator)) {
				return nil, fmt.Errorf("%s also holds another session, so deja does not move it", f)
			}
		}
	}
	shared := 0
	for _, o := range others {
		if o == s.Path {
			shared++
		}
	}
	if shared > 1 {
		if !isTranscriptFile(s.Path) {
			return nil, errNotFileBacked
		}
		return nil, fmt.Errorf("%s holds %d sessions, so deja does not move it", s.Path, shared)
	}
	return existing(files), nil
}

var errNotFileBacked = errors.New("not a file of its own")

var errNotDeletable = errors.New("not checked")

// deletableRefusal says why a session stays, and what to do instead.
func deletableRefusal(harness string) error {
	if harness == "deja" {
		return errors.New("this is a deja note, not an agent's session; `deja forget --session <id>` removes it")
	}
	return fmt.Errorf("deja can't delete %s sessions yet: it has not checked how %s lists them", agentName(harness), agentName(harness))
}

// deletable are the agents whose session files were checked to be what the
// agent itself lists sessions from, so moving them takes the session out of
// the agent and moving them back returns it. Others are left alone until
// someone checks them: VS Code extensions keep their chat index in VS Code's
// own state, Aider keeps a project's whole history in one file, Amp's threads
// live on its servers.
var deletable = map[string]bool{
	"claude": true, "codex": true, "gemini": true, "qwen": true, "copilot": true,
	"cline": true, "cursor": true, "kimi": true, "grok": true, "deepseek": true,
	"pi": true, "senpi": true, "openclaw": true, "antigravity": true,
}

// companions are the other places an agent keeps the same session. Moving
// only the transcript deja reads would leave the agent listing it: Cursor's
// CLI keeps the conversation in chats/<hash>/<id>/store.db and the transcript
// is a copy, Antigravity keeps it in conversations/<id>.db beside brain/<id>,
// and Codex compresses a week-old rollout or archives it.
func companions(s model.Session, files []string) []string {
	var out []string
	switch s.Harness {
	case "codex":
		out = codexRollouts(s.ID, s.Path)
	case "cursor":
		home, _ := os.UserHomeDir()
		m, _ := filepath.Glob(filepath.Join(home, ".cursor", "chats", "*", s.ID))
		out = m
	case "antigravity":
		for _, f := range files {
			if filepath.Base(filepath.Dir(f)) == "brain" {
				base := filepath.Join(filepath.Dir(filepath.Dir(f)), "conversations", s.ID+".db")
				out = append(out, base, base+"-shm", base+"-wal")
			}
		}
	}
	if len(s.ID) < 8 || strings.ContainsAny(s.ID, `*?[\/`) {
		return nil
	}
	return out
}

// sessionDir is the highest directory above the transcript whose name carries
// the session's id, or "". An id shorter than eight characters names nothing
// reliably, and the walk stops at the home directory.
func sessionDir(s model.Session) string {
	if len(s.ID) < 8 {
		return ""
	}
	home, _ := os.UserHomeDir()
	found := ""
	for d := filepath.Dir(s.Path); d != filepath.Dir(d) && d != home; d = filepath.Dir(d) {
		if strings.Contains(filepath.Base(d), s.ID) {
			found = d
		}
	}
	return found
}

func codexRollouts(id, have string) []string {
	if id == "" || strings.ContainsAny(id, `*?[\/`) {
		return nil
	}
	var out []string
	for _, root := range sources.CodexRoots() {
		for _, pattern := range []string{
			filepath.Join(root, "sessions", "*", "*", "*", "rollout-*"+id+".jsonl*"),
			filepath.Join(root, "archived_sessions", "rollout-*"+id+".jsonl*"),
		} {
			m, _ := filepath.Glob(pattern)
			for _, p := range m {
				if p != have {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

func existing(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// cliStore is an agent whose sessions live in its own database and that can
// export a session, delete it and import it back under the same id: the
// export is the trash and the import the undo. Measured on opencode 1.x: the
// re-export after delete and import equals the first one. goose imports under
// a new id and Hermes and Zed cannot import, so those are refused.
type cliStore struct{ bin string }

var cliStores = map[string]cliStore{"opencode": {"opencode"}}

func (c cliStore) run(args ...string) ([]byte, error) {
	cmd := exec.Command(c.bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %v %s", c.bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// trashSession moves a session into the trash. A running session is refused:
// its agent would write it again on the next turn.
func trashSession(dir string, s model.Session, others []string) (trashEntry, error) {
	if liveSessionIDs(dir)[s.ID] {
		return trashEntry{}, fmt.Errorf("session %s is running right now; end it first", digest.Short(s.ID))
	}
	ed := trashEntryDir(dir, s.Harness, s.ID)
	if _, err := os.Lstat(ed); err == nil {
		return trashEntry{}, fmt.Errorf("session %s is already in the trash; `deja delete --undo %s` puts it back first", digest.Short(s.ID), digest.Short(s.ID))
	}
	e := trashEntry{Harness: s.Harness, ID: s.ID, Title: s.Title, At: time.Now().UTC()}
	if c, ok := cliStores[s.Harness]; ok {
		return trashViaCLI(ed, c, e)
	}
	files, err := deletableFiles(s, others)
	if errors.Is(err, errNotDeletable) {
		return trashEntry{}, deletableRefusal(s.Harness)
	}
	if errors.Is(err, errNotFileBacked) {
		return trashEntry{}, fmt.Errorf("%s keeps its sessions in a database deja cannot take one out of and put back, so deja does not delete them", agentName(s.Harness))
	}
	if err != nil {
		return trashEntry{}, err
	}
	if len(files) == 0 {
		return trashEntry{}, fmt.Errorf("session %s is already gone from %s", digest.Short(s.ID), agentName(s.Harness))
	}
	if err := os.MkdirAll(ed, 0o700); err != nil {
		return trashEntry{}, err
	}
	for i, from := range files {
		name := fmt.Sprintf("%d-%s", i, filepath.Base(from))
		if err := moveFile(from, filepath.Join(ed, name)); err != nil {
			// Put back what already moved, so a half-deleted session is not
			// left behind.
			for _, f := range e.Files {
				_ = moveFile(filepath.Join(ed, f.Name), f.From)
			}
			_ = os.RemoveAll(ed)
			return trashEntry{}, fmt.Errorf("could not move %s: %w", from, err)
		}
		e.Files = append(e.Files, trashFile{From: from, Name: name})
	}
	return e, writeTrashEntry(ed, e)
}

func trashViaCLI(ed string, c cliStore, e trashEntry) (trashEntry, error) {
	if _, err := exec.LookPath(c.bin); err != nil {
		return trashEntry{}, fmt.Errorf("deleting a %s session goes through its own command, and %s is not on PATH", agentName(e.Harness), c.bin)
	}
	out, err := c.run("export", e.ID)
	if err != nil {
		return trashEntry{}, err
	}
	// The export is the only copy once the delete runs: it has to be the
	// session, whole, before anything is removed.
	var probe struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if i := strings.IndexByte(string(out), '{'); i < 0 || json.Unmarshal(out[i:], &probe) != nil || probe.Info.ID != e.ID {
		return trashEntry{}, fmt.Errorf("%s export of %s did not come back as that session, so nothing was deleted", c.bin, digest.Short(e.ID))
	} else {
		out = out[i:]
	}
	if err := os.MkdirAll(ed, 0o700); err != nil {
		return trashEntry{}, err
	}
	if err := os.WriteFile(filepath.Join(ed, "export.json"), out, 0o600); err != nil {
		_ = os.RemoveAll(ed)
		return trashEntry{}, err
	}
	e.Export = "export.json"
	if err := writeTrashEntry(ed, e); err != nil {
		_ = os.RemoveAll(ed)
		return trashEntry{}, err
	}
	if _, err := c.run("session", "delete", e.ID); err != nil {
		_ = os.RemoveAll(ed)
		return trashEntry{}, err
	}
	return e, nil
}

func writeTrashEntry(ed string, e trashEntry) error {
	b, _ := json.MarshalIndent(e, "", "  ")
	return os.WriteFile(filepath.Join(ed, "entry.json"), b, 0o600)
}

// untrashSession moves the files back where they were. A path the agent has
// written again since is left alone and the entry kept, so nothing is lost
// either way.
func untrashSession(dir, harness, id string) (trashEntry, error) {
	ed := trashEntryDir(dir, harness, id)
	e, err := readTrashEntry(ed)
	if err != nil {
		return e, err
	}
	if e.Export != "" {
		c, ok := cliStores[e.Harness]
		if !ok {
			return e, fmt.Errorf("no way to import a %s session back; the export is %s", e.Harness, filepath.Join(ed, e.Export))
		}
		if _, err := c.run("import", filepath.Join(ed, e.Export)); err != nil {
			return e, err
		}
		return e, os.RemoveAll(ed)
	}
	for _, f := range e.Files {
		if _, err := os.Lstat(f.From); err == nil {
			return e, fmt.Errorf("%s exists again, so deja did not overwrite it; the deleted copy is in %s", f.From, ed)
		}
	}
	for _, f := range e.Files {
		if err := os.MkdirAll(filepath.Dir(f.From), 0o755); err != nil {
			return e, err
		}
		if err := moveFile(filepath.Join(ed, f.Name), f.From); err != nil {
			return e, fmt.Errorf("could not put back %s: %w", f.From, err)
		}
	}
	return e, os.RemoveAll(ed)
}

func readTrashEntry(ed string) (trashEntry, error) {
	var e trashEntry
	b, err := os.ReadFile(filepath.Join(ed, "entry.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return e, errNotInTrash
		}
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("%s: %w", ed, err)
	}
	return e, nil
}

var errNotInTrash = errors.New("not in deja's trash")

// trashed lists the trash, newest first.
func trashed(dir string) []trashEntry {
	var out []trashEntry
	hs, _ := os.ReadDir(trashDir(dir))
	for _, h := range hs {
		ids, _ := os.ReadDir(filepath.Join(trashDir(dir), h.Name()))
		for _, id := range ids {
			if e, err := readTrashEntry(filepath.Join(trashDir(dir), h.Name(), id.Name())); err == nil {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// emptyTrash lets go of what has been in the trash longer than trashKeep.
func emptyTrash(dir string, now time.Time) int {
	n := 0
	for _, e := range trashed(dir) {
		if now.Sub(e.At) > trashKeep {
			if os.RemoveAll(trashEntryDir(dir, e.Harness, e.ID)) == nil {
				n++
			}
		}
	}
	return n
}

// moveFile renames, and copies then removes across filesystems.
func moveFile(from, to string) error {
	err := os.Rename(from, to)
	if err == nil || !isCrossDevice(err) {
		return err
	}
	if err := copyTree(from, to); err != nil {
		_ = os.RemoveAll(to)
		return err
	}
	return os.RemoveAll(from)
}

func isCrossDevice(err error) bool {
	var le *os.LinkError
	if !errors.As(err, &le) {
		return false
	}
	// EXDEV on Unix; ERROR_NOT_SAME_DEVICE on Windows.
	msg := strings.ToLower(le.Err.Error())
	return strings.Contains(msg, "cross-device") || strings.Contains(msg, "different disk drive")
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		info, _ := d.Info()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, src); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

var deleteFlags = []string{"--undo", "--list"}

func runDelete(dir string, args []string, w io.Writer) error {
	undo, list := false, false
	prefix := ""
	for _, a := range args {
		switch a {
		case "--undo":
			undo = true
		case "--list":
			list = true
		default:
			if strings.HasPrefix(a, "-") {
				return unknownFlag("delete", a, deleteFlags)
			}
			if prefix != "" {
				return fmt.Errorf("delete takes one session id; got %q and %q", prefix, a)
			}
			prefix = a
		}
	}
	emptyTrash(dir, time.Now())
	if list {
		es := trashed(dir)
		if len(es) == 0 {
			fmt.Fprintln(w, "the trash is empty")
			return nil
		}
		for _, e := range es {
			left := int(math.Ceil((trashKeep - time.Since(e.At)).Hours() / 24))
			fmt.Fprintf(w, "[%s] %s  %s  %s — kept %d more day%s\n", e.Harness, digest.Short(e.ID), e.At.Local().Format("Jan 2"), e.Title, left, pluralS(left))
		}
		return nil
	}
	if prefix == "" {
		return idPrefixNeeded(dir, "delete needs a session id prefix", "delete needs a session id prefix (see `deja last`)")
	}
	if undo {
		return runUndelete(dir, prefix, w)
	}
	if n := index.PrefixMatchesAllowed(dir, prefix, searchAllows()); n > 1 {
		return fmt.Errorf("%d sessions match %q — delete moves one; use a longer prefix (`deja last` prints ids whole)", n, prefix)
	}
	s, ok, err := findByPrefix(dir, prefix)
	if err != nil {
		return err
	}
	if !ok {
		return noSessionMatches(dir, prefix)
	}
	if _, err := trashSession(dir, s, indexedPaths(dir)); err != nil {
		return err
	}
	fmt.Fprintf(w, "deleted %s from %s: %s\n", digest.Short(s.ID), agentName(s.Harness), s.Title)
	fmt.Fprintf(w, "it is in deja's trash for %d days — `deja delete --undo %s` puts it back as it was\n",
		int(trashKeep.Hours()/24), digest.Short(s.ID))
	return nil
}

// indexedPaths is every indexed session's path, for the check that a move
// takes no other session with it.
func indexedPaths(dir string) []string {
	ss, _, err := index.RecentMatchingCounted(dir, 0, search.Options{})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Path)
	}
	return out
}

// runUndelete matches the prefix against the trash itself: the session is
// gone from its agent, and the trash is what remembers where it lived.
func runUndelete(dir, prefix string, w io.Writer) error {
	harness, id, _ := strings.Cut(prefix, ":")
	if id == "" {
		harness, id = "", prefix
	}
	var hit []trashEntry
	for _, e := range trashed(dir) {
		if strings.HasPrefix(e.ID, id) && (harness == "" || e.Harness == harness) {
			hit = append(hit, e)
		}
	}
	switch len(hit) {
	case 0:
		return fmt.Errorf("no session in deja's trash matches %q — `deja delete --list` shows what is there", prefix)
	case 1:
	default:
		return fmt.Errorf("%d sessions in the trash match %q; use a longer prefix", len(hit), prefix)
	}
	e, err := untrashSession(dir, hit[0].Harness, hit[0].ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "put %s back in %s: %s\n", digest.Short(e.ID), agentName(e.Harness), e.Title)
	return nil
}
