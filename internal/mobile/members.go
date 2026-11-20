package mobile

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/members"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

// The phone joins "my devices" as a controller only: it pairs through a
// device-code connection to a desktop, then dials members directly. It
// syncs outbound while in the foreground and never listens.

// secretKeys keeps member secrets in the platform's secure store (the
// Android Keystore). Slot names are not secret; an index of them lives in
// the store too, since it cannot list its items.
type secretKeys struct {
	mu sync.Mutex
	s  core.Secrets
}

const indexAccount = "members-index"

func account(slot string) string { return "members:" + slot }

func (k *secretKeys) index() []string {
	v, ok, _ := k.s.Get(indexAccount)
	var out []string
	if ok {
		json.Unmarshal([]byte(v), &out)
	}
	return out
}

func (k *secretKeys) setIndex(slots []string) error {
	b, _ := json.Marshal(slots)
	return k.s.Set(indexAccount, string(b))
}

func (k *secretKeys) Put(slot string, secret []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	idx := k.index()
	if !slices.Contains(idx, slot) { // index first: a crash leaves at most an orphan entry
		if err := k.setIndex(append(idx, slot)); err != nil {
			return err
		}
	}
	return k.s.Set(account(slot), base64.StdEncoding.EncodeToString(secret))
}

func (k *secretKeys) Get(slot string) ([]byte, error) {
	v, ok, err := k.s.Get(account(slot))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, members.ErrNoSlot
	}
	return base64.StdEncoding.DecodeString(v)
}

func (k *secretKeys) Delete(slot string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.s.Delete(account(slot)); err != nil {
		return err
	}
	return k.setIndex(slices.DeleteFunc(k.index(), func(s string) bool { return s == slot }))
}

func (k *secretKeys) List() ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.index(), nil
}

type pairPrompt struct {
	p  members.PairPrompt
	ch chan bool
}

func (a *app) openMembers() {
	a.mem = core.NewSignalFunc(a.cx, members.Snapshot{}, func(x, y members.Snapshot) bool { return false })
	a.prompt = core.NewSignal[*pairPrompt](a.cx, nil)
	a.pairing = core.NewSignal(a.cx, "")
	dir := a.cx.DataDir()
	var keys members.KeyStore = &secretKeys{s: a.cx.Secrets()}
	if dir == "" || runtime.GOOS != "android" { // RG_MOBILE=1 on a desktop
		base, _ := os.UserConfigDir()
		dir = filepath.Join(base, "remote-go-mobile")
		keys = members.FileKeyStore{Dir: filepath.Join(dir, "member-keys")}
	}
	st, err := members.OpenStore(dir)
	if err != nil {
		log.Printf("我的设备不可用：%v", err)
		return
	}
	n, err := members.OpenNode(st, keys)
	if err != nil {
		st.Close()
		log.Printf("我的设备不可用：%v", err)
		return
	}
	a.node = n
	n.Changed = func() { a.cx.Post(a.refreshMembers) }
	a.refreshMembers()
}

func (a *app) refreshMembers() {
	if a.node != nil {
		a.mem.Set(a.node.Snapshot())
	}
}

// syncMembers pulls from reachable desktops, as the app comes forward.
func (a *app) syncMembers() {
	if a.node == nil {
		return
	}
	n, logf := a.node, tunnel.Logf(a.verbose)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		backend.SyncAll(ctx, n, logf)
	}()
}

func (a *app) memberName(id string) string {
	for _, m := range a.mem.GetUntracked().Members {
		if m.ID.String() == id && m.Name != "" {
			return m.Name
		}
	}
	return "我的设备"
}

// pairWith adds the desktop with these codes; the phone joins its group
// (or, if neither has one, creates it and invites the desktop).
func (a *app) pairWith(code, pw string) {
	if a.node == nil {
		a.toast(errors.New("我的设备不可用"))
		return
	}
	c := tunnel.Normalize(code)
	if !tunnel.Valid(c) || strings.TrimSpace(pw) == "" {
		a.toast(errors.New("请先填写要添加的电脑的设备代码和验证码"))
		return
	}
	n, relay, logf := a.node, a.settings.GetUntracked().RelayAddress, tunnel.Logf(a.verbose)
	opts := members.PairOptions{Name: phoneName(), Platform: "android", CanInbound: false,
		Confirm: func(ctx context.Context, p members.PairPrompt) (bool, bool) {
			pp := &pairPrompt{p: p, ch: make(chan bool, 1)}
			a.cx.Post(func() { a.prompt.Set(pp) })
			defer a.cx.Post(func() {
				if a.prompt.GetUntracked() == pp {
					a.prompt.Set(nil)
				}
			})
			select {
			case ok := <-pp.ch:
				return ok, false
			case <-ctx.Done():
				return false, false
			}
		}}
	a.pairing.Set("正在配对，请核对两台设备上的配对码…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		err := backend.PairDial(ctx, n, c, strings.TrimSpace(pw), relay, opts, logf)
		a.cx.Post(func() {
			a.pairing.Set("")
			if err != nil {
				a.toast(errors.New("配对未完成：" + err.Error()))
				return
			}
			a.notice("已添加到我的设备")
			a.refreshMembers()
		})
	}()
}

func phoneName() string {
	if h, err := os.Hostname(); err == nil && h != "" && h != "localhost" {
		return h
	}
	return "Android 手机"
}

// devicesCard lists desktops this phone can reach without codes.
func (a *app) devicesCard(cx *core.Scope, code, pw func() string) core.Node {
	desktops := func() []members.MemberInfo {
		var out []members.MemberInfo
		for _, m := range a.mem.Get().Members {
			if !m.Self && m.Inbound {
				out = append(out, m)
			}
		}
		return out
	}
	return core.View{Class: "gap-2", Children: []core.Node{
		core.View{Class: "flex-row items-center", Children: []core.Node{
			text("flex-1 text-base font-semibold", "我的设备"),
			core.Show{When: func() bool { return a.mem.Get().InGroup }, Then: func(*core.Scope) core.Node {
				return ui.IconButton{Icon: lucide.RefreshCw, Label: "同步", OnPress: a.syncMembers}
			}},
		}},
		core.View{Class: "border-t border-border overflow-hidden", Children: []core.Node{
			core.Show{When: func() bool { return a.mem.Get().Conflict }, Then: func(*core.Scope) core.Node {
				return core.View{Class: "py-3 gap-2 border-b border-border", Children: []core.Node{
					text("text-sm text-red-500", "设备组出现互相矛盾的修改，已暂停免码连接。请在电脑上重建设备组后重新添加本机。"),
					ui.Button{Text: "在本机停用旧设备组", Variant: ui.ButtonOutline, OnPress: func() {
						if err := a.node.Forget(); err != nil {
							a.toast(err)
						}
					}},
				}}
			}},
			core.Each(desktops, func(m members.MemberInfo) string { return m.ID.String() }, func(cx *core.Scope, m func() members.MemberInfo, _ func() int) core.Node {
				return core.View{Label: "连接", OnPress: func(core.PressEvent) {
					a.notice("正在连接，电脑上可能需要本人允许…")
					a.openRemote("", "", m().ID.String(), func(err error) {
						if err != nil {
							a.toast(err)
						}
					})
				}, Class: "flex-row items-center gap-3 py-3 border-b border-border", Children: []core.Node{
					icon(lucide.Laptop, "w-[22px] h-[22px] text-muted-foreground"),
					core.View{Class: "flex-1 gap-0.5", Children: []core.Node{
						textf("text-base font-medium", func() string { return m().Name }),
						textf("text-xs text-muted-foreground", func() string {
							s := osName(m().Platform) + " · " + m().ID.Short()
							if !m().Synced {
								s += " · 待同步"
							}
							return s
						}),
					}},
					ui.IconButton{Icon: lucide.X, Label: "移除", OnPress: func() {
						if _, err := a.node.Remove(m().ID, true); err != nil {
							a.toast(err)
						} else {
							a.syncMembers()
						}
					}},
				}}
			}),
			core.View{Class: "py-3 gap-2", Children: []core.Node{
				textf("text-xs text-muted-foreground", func() string {
					switch {
					case a.pairing.Get() != "":
						return a.pairing.Get()
					case a.node == nil:
						return "我的设备不可用"
					case !a.mem.Get().InGroup:
						return "填写上方的设备代码和验证码，然后添加设备。配对后可直接连接。"
					}
					return "添加其他电脑：填好它的设备代码和验证码后点下方按钮。"
				}),
				ui.Button{Text: "添加到我的设备", Variant: ui.ButtonOutline, Disabled: func() bool { return a.node == nil || a.pairing.Get() != "" },
					OnPress: func() { a.pairWith(code(), pw()) }},
				core.Show{When: func() bool { return a.mem.Get().InGroup }, Then: func(*core.Scope) core.Node {
					return ui.Button{Text: "退出设备组", Variant: ui.ButtonGhost, OnPress: func() {
						n := a.node
						go func() {
							n.Leave()
							ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
							backend.SyncAll(ctx, n, nil)
							cancel()
							err := n.Forget()
							a.cx.Post(func() {
								if err != nil {
									a.toast(err)
								}
							})
						}()
					}}
				}},
			}},
		}},
		a.pairDialog(cx),
	}}
}

func (a *app) pairDialog(cx *core.Scope) core.Node {
	answer := func(ok bool) {
		if p := a.prompt.GetUntracked(); p != nil {
			p.ch <- ok
			a.prompt.Set(nil)
		}
	}
	return ui.Dialog{
		Open:                   func() bool { return a.prompt.Get() != nil },
		OnOpenChange:           func(bool, base.Reason) {},
		NoCloseOnEscape:        true,
		NoCloseOnBackdropPress: true,
		Title:                  "核对配对码",
		DescriptionFunc: func() string {
			if p := a.prompt.Get(); p != nil {
				return "确认「" + p.p.PeerName + "」上显示的配对码与下方完全一致。"
			}
			return ""
		},
		Children: []core.Node{
			textf("text-2xl font-semibold text-center py-2", func() string {
				if p := a.prompt.Get(); p != nil {
					return p.p.Code
				}
				return ""
			}),
		},
		Footer: []core.Node{
			ui.Button{Text: "不一致", Variant: ui.ButtonOutline, OnPress: func() { answer(false) }},
			ui.Button{Text: "一致，确认", OnPress: func() { answer(true) }},
		},
	}
}
