package mobile

import (
	"strings"
	"unicode"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// keyboardBar sits above the soft keyboard. What the user types goes to
// the host as text as soon as the keyboard commits it (an IME's
// composition stays local until chosen), so Chinese works whatever the
// host's layout. Keys a soft keyboard lacks have their own buttons, and
// Ctrl, Alt, Shift and ⌘/Win stick for the next key, for shortcuts.
func (a *app) keyboardBar(cx *core.Scope, r *remote) core.Node {
	buf := core.NewSignal(cx, "")
	composing := false
	mods := core.NewSignal(cx, uint8(0)) // bit i: modCodes[i] is held
	ref := &core.Ref{}

	held := func() []string {
		var out []string
		for i, m := range modCodes {
			if mods.GetUntracked()&(1<<i) != 0 {
				out = append(out, m)
			}
		}
		return out
	}
	press := func(code string) {
		s := r.sess()
		if s == nil {
			return
		}
		keys := append(held(), code)
		mods.Set(0)
		go s.SendKeys(keys...)
	}
	flush := func() {
		t := buf.GetUntracked()
		if t == "" {
			return
		}
		buf.Set("")
		s := r.sess()
		if s == nil {
			return
		}
		if len(held()) > 0 {
			// A shortcut: Ctrl+C and the like need key codes, not text.
			for _, ch := range t {
				if code := charCode(ch); code != "" {
					press(code)
				}
			}
			return
		}
		go s.Text(t)
	}

	modKey := func(label string, i int) core.Node {
		bit := uint8(1) << i
		return core.View{Role: core.RoleButton, Label: label, OnPress: func(core.PressEvent) {
			mods.Set(mods.GetUntracked() ^ bit)
		}, ClassFunc: func() string {
			if mods.Get()&bit != 0 {
				return "h-[36px] px-3 rounded-lg bg-primary items-center justify-center"
			}
			return "h-[36px] px-3 rounded-lg bg-muted items-center justify-center"
		}, Children: []core.Node{textf("text-sm", func() string { return label })}}
	}
	key := func(label, code string) core.Node {
		return core.View{Role: core.RoleButton, Label: label, OnPress: func(core.PressEvent) { flush(); press(code) },
			Class:    "h-[36px] min-w-[40px] px-3 rounded-lg bg-muted items-center justify-center",
			Children: []core.Node{text("text-sm", label)}}
	}
	meta := "Win"
	if r.t.OS == "darwin" {
		meta = "⌘"
	}

	return core.View{Class: "absolute left-0 right-0 bottom-0 bg-background border-t border-border pb-[env(safe-area-inset-bottom)] mb-[env(keyboard-inset-height)]", Children: []core.Node{
		core.Scroll{Horizontal: true, Style: core.Style{Height: core.Pt(48)}, Children: []core.Node{
			core.View{Class: "flex-row items-center gap-1.5 px-2 h-[48px]", Children: []core.Node{
				key("Esc", "Escape"), key("Tab", "Tab"),
				modKey("Ctrl", 0), modKey("Alt", 1), modKey("Shift", 2), modKey(meta, 3),
				key("←", "ArrowLeft"), key("↑", "ArrowUp"), key("↓", "ArrowDown"), key("→", "ArrowRight"),
				key("⌫", "Backspace"), key("Del", "Delete"), key("⏎", "Enter"),
				key("Home", "Home"), key("End", "End"), key("PgUp", "PageUp"), key("PgDn", "PageDown"),
				key("F5", "F5"), key("F11", "F11"),
			}},
		}},
		core.View{Class: "flex-row items-center gap-2 px-2 pb-2", Children: []core.Node{
			ui.Input{Placeholder: "在这里输入，实时发送到对方", Value: buf.Get, Ref: ref, AutoFocus: true, Label: "输入文字",
				NoAutoCorrect: true, AutoCapitalize: core.AutoCapitalizeNone, ReturnKeyType: core.ReturnKeySend,
				SubmitBehavior: core.SubmitBehaviorSubmit,
				OnCompositionChange: func(c core.Composition) {
					composing = c.Composing
					if !composing {
						flush()
					}
				},
				OnChange: func(t string) {
					buf.Set(t)
					if !composing {
						flush()
					}
				},
				OnKeyPress: func(k string) {
					if k == "Backspace" && buf.GetUntracked() == "" {
						press("Backspace")
					}
				},
				OnSubmit: func(string) { flush(); press("Enter") },
				Style:    core.Style{FlexGrow: 1, Height: core.Pt(40), FontSize: 16}},
			ui.IconButton{Icon: lucide.ChevronDown, Label: "收起键盘", OnPress: func() { r.kb.Set(false) }},
		}},
	}}
}

var modCodes = []string{"ControlLeft", "AltLeft", "ShiftLeft", "MetaLeft"}

// charCode is the KeyboardEvent.code typing ch on a US keyboard, for
// shortcuts; "" for characters without one.
func charCode(ch rune) string {
	switch {
	case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z':
		return "Key" + strings.ToUpper(string(unicode.ToLower(ch)))
	case ch >= '0' && ch <= '9':
		return "Digit" + string(ch)
	case ch == ' ':
		return "Space"
	}
	return ""
}
