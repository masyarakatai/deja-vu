package main

import (
	"strings"

	"github.com/vshulcz/deja-vu/internal/mark"
	"github.com/vshulcz/deja-vu/internal/model"
	"github.com/vshulcz/deja-vu/internal/tui"
)

// D deletes the selected session from its agent, the way `deja delete` does:
// into deja's trash, and R puts it back as it was.

func (a *tuiApp) askDelete() {
	s, ok := a.current()
	if !ok {
		return
	}
	if _, ok := cliStores[s.Harness]; !ok && !deletable[s.Harness] {
		a.say(deletableRefusal(s.Harness).Error(), false)
		return
	}
	if d := a.details[sessionKey(s)]; d != nil && d.gone {
		a.say(agentName(s.Harness)+" no longer has this one; R puts it back.", false)
		return
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
	paths := make([]string, 0, len(a.allMeta))
	for _, m := range a.allMeta {
		paths = append(paths, m.Path)
	}
	a.spawn(func() {
		_, err := trashSession(a.dir, s, paths)
		a.post(func() {
			if err != nil {
				a.say(err.Error(), false)
				return
			}
			if d := a.details[sessionKey(s)]; d != nil {
				d.gone = true
			}
			if !a.keptIDs[sessionKey(s)] {
				a.keptIDs[sessionKey(s)] = true
				a.kept = append([]model.Session{s}, a.kept...)
			}
			a.keptGen.Add(1)
			a.spawn(a.loadKept)
			a.say("Deleted from "+agentName(s.Harness)+". R puts it back.", true)
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
	p.PutClip(tx, ty, "Delete it from "+agentName(s.Harness)+"?", bold(cText), right)
	asked := strings.Join(strings.Fields(s.Title), " ")
	if asked == "" {
		asked = "(no prompt recorded)"
	}
	p.PutClip(tx, ty+1, asked, fgs(cText), right)
	p.PutClip(tx, ty+2, agentName(s.Harness)+" · "+tuiProject(s)+" · "+tuiAgo(s.Updated, a.now), fgs(cMuted), right)
	p.PutClip(tx, ty+4, agentName(s.Harness)+" stops listing it.", fgs(cSub), right)
	p.PutClip(tx, ty+5, "deja keeps it 30 days; R puts it back.", fgs(cSub), right)
	bx := p.button(tx, ty+7, "↵", "Delete", true, right)
	p.button(bx+2, ty+7, "esc", "Keep it", false, right)
}
