package main

import (
	"strings"

	"github.com/vshulcz/deja-vu/internal/mark"
	"github.com/vshulcz/deja-vu/internal/tui"
)

// D erases the selected session everywhere, the way `deja delete` does: its
// agent's files, then deja's copy through the same path F takes.

func (a *tuiApp) askDelete() {
	s, ok := a.current()
	if !ok {
		return
	}
	// Refuse before asking: a question answered with a refusal wastes the
	// yes. eraseSession checks again when it runs.
	if _, cli := cliStores[s.Harness]; !cli {
		if _, err := sessionFiles(s, a.allMeta); err != nil {
			a.say(err.Error(), false)
			return
		}
	}
	a.openModal(modalDelete)
	a.m.src = s
}

func (a *tuiApp) handleDelete(ev tui.Event) {
	switch ev.Key {
	case tui.KeyEnter:
		a.modal = modalNone
		a.deleteSession()
	case tui.KeyEsc:
		a.modal = modalNone
	case tui.KeyRune:
		if ev.Rune == 'q' || ev.Rune == 'n' {
			a.modal = modalNone
		}
	}
}

func (a *tuiApp) deleteSession() {
	s := a.m.src
	others := a.allMeta
	a.say("Erasing "+agentName(s.Harness)+"'s copy…", true)
	a.spawn(func() {
		_, err := eraseSession(a.dir, s, others)
		a.post(func() {
			if err != nil {
				a.say(err.Error(), false)
				return
			}
			// Gone from the agent; now from deja, the way F does it.
			a.forgetSession(s)
		})
	})
}

func (a *tuiApp) drawDelete() {
	p := a.p
	s := a.m.src
	x, y, iw := a.modalBox(80, 15)
	right := x + iw
	tx, ty := x, y
	if iw >= 64 && p.H >= 17 {
		a.drawCat(x+1, y+1, mark.Nothing)
		tx, ty = x+28, y+3
	}
	p.PutClip(tx, ty, "Delete this session everywhere?", bold(cText), right)
	asked := strings.Join(strings.Fields(s.Title), " ")
	if asked == "" {
		asked = "(no prompt recorded)"
	}
	p.PutClip(tx, ty+1, asked, fgs(cText), right)
	p.PutClip(tx, ty+2, agentName(s.Harness)+" · "+tuiProject(s)+" · "+tuiAgo(s.Updated, a.now), fgs(cMuted), right)
	p.PutClip(tx, ty+4, "Gone from "+agentName(s.Harness)+" and from deja.", fgs(cSub), right)
	p.PutClip(tx, ty+5, "This cannot be undone.", bold(cPeach), right)
	bx := p.button(tx, ty+7, "↵", "Delete", true, right)
	p.button(bx+2, ty+7, "esc", "Keep it", false, right)
}
