//go:build linux && !android

package screen

// Linux remote-screen view: a GtkOverlay that NSGo's GTK 4 backend hosts,
// holding a GtkPicture of the decoded frames (letterboxed, GPU-scaled by
// GSK) and the session's round control button. Frames decode with
// GStreamer (encoder.H264Decoder) or Go's JPEG decoder; input is taken by
// evdev keycode, so the remote keyboard is layout-independent.

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	"log"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/nsgo-dev/nsgo/core"

	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/controlball"
	"github.com/i5shuyi/remote-go/internal/encoder"
	"github.com/i5shuyi/remote-go/internal/keymap"
)

const (
	gdkMemoryB8G8R8A8Premultiplied = 0
	gdkMemoryR8G8B8A8Premultiplied = 2

	gtkAlignEnd   = 2
	gtkAlignStart = 1

	gtkScrollBothAxes  = 3
	gdkScrollUnitWheel = 0

	wheelPixels = 100 // browser pixels per wheel notch
)

var g struct {
	once sync.Once
	ok   bool

	ref              func(uintptr) uintptr
	refSink          func(uintptr) uintptr
	unref            func(uintptr)
	signalConnect    func(instance uintptr, signal string, handler, data, destroy uintptr, flags uint32) uint64
	idleAdd          func(fn, data uintptr) uint32
	bytesNew         func(data unsafe.Pointer, size uintptr) uintptr
	bytesUnref       func(uintptr)
	memoryTextureNew func(w, h int32, format int32, bytes uintptr, stride uintptr) uintptr

	overlayNew            func() uintptr
	overlaySetChild       func(overlay, child uintptr)
	overlayAddOverlay     func(overlay, child uintptr)
	pictureNew            func() uintptr
	pictureSetPaintable   func(picture, paintable uintptr)
	pictureSetCanShrink   func(picture uintptr, on int32)
	buttonNewWithLabel    func(label string) uintptr
	widgetAddCSSClass     func(w uintptr, class string)
	widgetSetHalign       func(w uintptr, a int32)
	widgetSetValign       func(w uintptr, a int32)
	widgetSetMarginTop    func(w uintptr, m int32)
	widgetSetMarginEnd    func(w uintptr, m int32)
	widgetSetFocusable    func(w uintptr, on int32)
	widgetSetFocusOnClick func(w uintptr, on int32)
	widgetSetTooltip      func(w uintptr, text string)
	widgetGrabFocus       func(w uintptr) int32
	widgetGetWidth        func(w uintptr) int32
	widgetGetHeight       func(w uintptr) int32
	widgetGetParent       func(w uintptr) uintptr
	widgetGetRoot         func(w uintptr) uintptr
	widgetAddController   func(w, c uintptr)
	widgetTranslate       func(src, dest uintptr, x, y float64, dx, dy *float64) int32

	keyNew          func() uintptr
	clickNew        func() uintptr
	motionNew       func() uintptr
	scrollNew       func(flags uint32) uintptr
	scrollGetUnit   func(c uintptr) int32 // GTK 4.8+, nil before
	focusNew        func() uintptr
	singleSetButton func(gesture uintptr, button uint32)
	singleGetButton func(gesture uintptr) uint32

	cssProviderNew func() uintptr
	cssLoadString  func(p uintptr, css string)        // GTK 4.12+
	cssLoadData    func(p uintptr, css string, n int) // older
	displayDefault func() uintptr
	addProvider    func(display, provider uintptr, priority uint32)

	cbIdle, cbMotion, cbPressed, cbReleased, cbScroll, cbKeyDown, cbKeyUp, cbLeave, cbClicked, cbMap uintptr
}

func loadGtk() bool {
	g.once.Do(func() {
		open := func(names ...string) uintptr {
			for _, n := range names {
				if h, err := purego.Dlopen(n, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
					return h
				}
			}
			return 0
		}
		gtk := open("libgtk-4.so.1", "libgtk-4.so")
		gobj := open("libgobject-2.0.so.0", "libgobject-2.0.so")
		glib := open("libglib-2.0.so.0", "libglib-2.0.so")
		if gtk == 0 || gobj == 0 || glib == 0 {
			log.Printf("远程屏幕：无法加载 GTK 4")
			return
		}
		for _, f := range []struct {
			lib uintptr
			fn  any
			sym string
		}{
			{gobj, &g.ref, "g_object_ref"},
			{gobj, &g.refSink, "g_object_ref_sink"},
			{gobj, &g.unref, "g_object_unref"},
			{gobj, &g.signalConnect, "g_signal_connect_data"},
			{glib, &g.idleAdd, "g_idle_add"},
			{glib, &g.bytesNew, "g_bytes_new"},
			{glib, &g.bytesUnref, "g_bytes_unref"},
			{gtk, &g.memoryTextureNew, "gdk_memory_texture_new"},
			{gtk, &g.overlayNew, "gtk_overlay_new"},
			{gtk, &g.overlaySetChild, "gtk_overlay_set_child"},
			{gtk, &g.overlayAddOverlay, "gtk_overlay_add_overlay"},
			{gtk, &g.pictureNew, "gtk_picture_new"},
			{gtk, &g.pictureSetPaintable, "gtk_picture_set_paintable"},
			{gtk, &g.pictureSetCanShrink, "gtk_picture_set_can_shrink"},
			{gtk, &g.buttonNewWithLabel, "gtk_button_new_with_label"},
			{gtk, &g.widgetAddCSSClass, "gtk_widget_add_css_class"},
			{gtk, &g.widgetSetHalign, "gtk_widget_set_halign"},
			{gtk, &g.widgetSetValign, "gtk_widget_set_valign"},
			{gtk, &g.widgetSetMarginTop, "gtk_widget_set_margin_top"},
			{gtk, &g.widgetSetMarginEnd, "gtk_widget_set_margin_end"},
			{gtk, &g.widgetSetFocusable, "gtk_widget_set_focusable"},
			{gtk, &g.widgetSetFocusOnClick, "gtk_widget_set_focus_on_click"},
			{gtk, &g.widgetSetTooltip, "gtk_widget_set_tooltip_text"},
			{gtk, &g.widgetGrabFocus, "gtk_widget_grab_focus"},
			{gtk, &g.widgetGetWidth, "gtk_widget_get_width"},
			{gtk, &g.widgetGetHeight, "gtk_widget_get_height"},
			{gtk, &g.widgetGetParent, "gtk_widget_get_parent"},
			{gtk, &g.widgetGetRoot, "gtk_widget_get_root"},
			{gtk, &g.widgetAddController, "gtk_widget_add_controller"},
			{gtk, &g.widgetTranslate, "gtk_widget_translate_coordinates"},
			{gtk, &g.keyNew, "gtk_event_controller_key_new"},
			{gtk, &g.clickNew, "gtk_gesture_click_new"},
			{gtk, &g.motionNew, "gtk_event_controller_motion_new"},
			{gtk, &g.scrollNew, "gtk_event_controller_scroll_new"},
			{gtk, &g.focusNew, "gtk_event_controller_focus_new"},
			{gtk, &g.singleSetButton, "gtk_gesture_single_set_button"},
			{gtk, &g.singleGetButton, "gtk_gesture_single_get_current_button"},
			{gtk, &g.cssProviderNew, "gtk_css_provider_new"},
			{gtk, &g.displayDefault, "gdk_display_get_default"},
			{gtk, &g.addProvider, "gtk_style_context_add_provider_for_display"},
		} {
			if _, err := purego.Dlsym(f.lib, f.sym); err != nil {
				log.Printf("远程屏幕：GTK 缺少 %s", f.sym)
				return
			}
			purego.RegisterLibFunc(f.fn, f.lib, f.sym)
		}
		optional := func(fn any, sym string) {
			if _, err := purego.Dlsym(gtk, sym); err == nil {
				purego.RegisterLibFunc(fn, gtk, sym)
			}
		}
		optional(&g.scrollGetUnit, "gtk_event_controller_scroll_get_unit")
		optional(&g.cssLoadString, "gtk_css_provider_load_from_string")
		if g.cssLoadString == nil {
			optional(&g.cssLoadData, "gtk_css_provider_load_from_data")
		}
		// purego keeps every trampoline for the process's life: one each,
		// shared by all views, which they find by the id in user data.
		g.cbIdle = purego.NewCallback(idleCallback)
		g.cbMotion = purego.NewCallback(motionCallback)
		g.cbPressed = purego.NewCallback(pressedCallback)
		g.cbReleased = purego.NewCallback(releasedCallback)
		g.cbScroll = purego.NewCallback(scrollCallback)
		g.cbKeyDown = purego.NewCallback(keyDownCallback)
		g.cbKeyUp = purego.NewCallback(keyUpCallback)
		g.cbLeave = purego.NewCallback(leaveCallback)
		g.cbClicked = purego.NewCallback(clickedCallback)
		g.cbMap = purego.NewCallback(mapCallback)
		g.ok = true
		styleOnce()
	})
	return g.ok
}

func styleOnce() {
	display := g.displayDefault()
	if display == 0 {
		return
	}
	css := `.remotego-screen { background-color: black; }
.remotego-ball { min-width: 40px; min-height: 40px; padding: 0; font-weight: bold; }`
	p := g.cssProviderNew()
	switch {
	case g.cssLoadString != nil:
		g.cssLoadString(p, css)
	case g.cssLoadData != nil:
		g.cssLoadData(p, css, -1)
	default:
		return
	}
	g.addProvider(display, p, 600) // GTK_STYLE_PROVIDER_PRIORITY_APPLICATION
}

var (
	views  = map[uintptr]*gtkView{} // by id; main thread only
	nextID uintptr
)

type gtkView struct {
	viewBase
	id                     uintptr
	cx                     *core.NativeContext
	overlay, picture, ball uintptr

	mu        sync.Mutex // guards the fields below, written off the main thread
	w, h      int        // video size
	pending   []byte     // the next frame to show
	format    int32
	pw, ph    int
	scheduled bool
	dec       *encoder.H264Decoder
	decErr    bool
}

func newView(cx *core.NativeContext) core.NativeComponent {
	if !loadGtk() {
		return nil
	}
	nextID++
	v := &gtkView{id: nextID, cx: cx}
	v.overlay = g.refSink(g.overlayNew()) // ours until Close, whatever NSGo does
	g.widgetAddCSSClass(v.overlay, "remotego-screen")

	v.picture = g.pictureNew()
	g.pictureSetCanShrink(v.picture, 1)
	g.widgetSetFocusable(v.picture, 1)
	g.overlaySetChild(v.overlay, v.picture)

	motion := g.motionNew()
	g.signalConnect(motion, "motion", g.cbMotion, v.id, 0, 0)
	g.widgetAddController(v.picture, motion)

	click := g.clickNew()
	g.singleSetButton(click, 0) // every button
	g.signalConnect(click, "pressed", g.cbPressed, v.id, 0, 0)
	g.signalConnect(click, "released", g.cbReleased, v.id, 0, 0)
	g.widgetAddController(v.picture, click)

	scroll := g.scrollNew(gtkScrollBothAxes)
	g.signalConnect(scroll, "scroll", g.cbScroll, v.id, 0, 0)
	g.widgetAddController(v.picture, scroll)

	keys := g.keyNew()
	g.signalConnect(keys, "key-pressed", g.cbKeyDown, v.id, 0, 0)
	g.signalConnect(keys, "key-released", g.cbKeyUp, v.id, 0, 0)
	g.widgetAddController(v.picture, keys)

	focus := g.focusNew()
	g.signalConnect(focus, "leave", g.cbLeave, v.id, 0, 0)
	g.widgetAddController(v.picture, focus)
	g.signalConnect(v.picture, "map", g.cbMap, v.id, 0, 0)

	v.ball = g.buttonNewWithLabel("•••")
	for _, c := range []string{"circular", "osd", "remotego-ball"} {
		g.widgetAddCSSClass(v.ball, c)
	}
	g.widgetSetTooltip(v.ball, "会话菜单")
	g.widgetSetFocusOnClick(v.ball, 0)
	g.widgetSetHalign(v.ball, gtkAlignEnd)
	g.widgetSetValign(v.ball, gtkAlignStart)
	g.widgetSetMarginTop(v.ball, 12)
	g.widgetSetMarginEnd(v.ball, 12)
	g.signalConnect(v.ball, "clicked", g.cbClicked, v.id, 0, 0)
	g.overlayAddOverlay(v.overlay, v.ball)

	views[v.id] = v
	return v
}

func (v *gtkView) View() any { return v.overlay }

func (v *gtkView) SetProp(name string, value any) {
	if name == "link" {
		id, _ := value.(string)
		v.setLink(v, id)
	}
}

func (v *gtkView) Command(name string, args any) {
	if name == "focus" {
		g.widgetGrabFocus(v.picture)
	}
}

func (v *gtkView) Close() {
	v.unlink(v)
	v.mu.Lock()
	if v.dec != nil {
		v.dec.Close()
		v.dec = nil
	}
	v.pending = nil
	v.mu.Unlock()
	delete(views, v.id)
	g.unref(v.overlay)
}

// frame runs on the session's goroutine.
func (v *gtkView) frame(kind byte, data []byte) bool {
	switch kind {
	case client.FrameJPEG:
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return true
		}
		b := img.Bounds()
		rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Rect, img, b.Min, draw.Src)
		v.show(rgba.Pix, b.Dx(), b.Dy(), gdkMemoryR8G8B8A8Premultiplied, false)
		return true
	case client.FrameH264Key, client.FrameH264Diff:
		v.mu.Lock()
		if v.dec == nil && !v.decErr {
			d, err := encoder.NewH264Decoder(func(p *encoder.Picture) {
				v.show(p.Pix, p.Width, p.Height, gdkMemoryB8G8R8A8Premultiplied, true)
			})
			if err != nil {
				log.Printf("H.264 解码器不可用：%v", err)
				v.decErr = true
			} else {
				v.dec = d
			}
		}
		d := v.dec
		v.mu.Unlock()
		if d == nil {
			return false
		}
		return d.Decode(data)
	}
	return true
}

func (v *gtkView) reset() {
	v.mu.Lock()
	d := v.dec
	v.mu.Unlock()
	if d != nil {
		d.Flush()
	}
}

// show hands a picture to the main thread, keeping only the newest.
func (v *gtkView) show(pix []byte, w, h int, format int32, copyPix bool) {
	if copyPix {
		pix = append([]byte(nil), pix...)
	}
	v.mu.Lock()
	v.pending, v.pw, v.ph, v.format = pix, w, h, format
	wake := !v.scheduled
	v.scheduled = true
	v.mu.Unlock()
	if wake {
		g.idleAdd(g.cbIdle, v.id)
	}
}

func idleCallback(id uintptr) int32 {
	v := views[id]
	if v == nil {
		return 0
	}
	v.mu.Lock()
	pix, w, h, format := v.pending, v.pw, v.ph, v.format
	v.pending, v.scheduled = nil, false
	if pix != nil {
		v.w, v.h = w, h
	}
	v.mu.Unlock()
	if pix == nil || len(pix) < w*h*4 {
		return 0
	}
	b := g.bytesNew(unsafe.Pointer(&pix[0]), uintptr(w*h*4)) // copies
	tex := g.memoryTextureNew(int32(w), int32(h), format, b, uintptr(w*4))
	g.bytesUnref(b)
	if tex != 0 {
		g.pictureSetPaintable(v.picture, tex)
		g.unref(tex)
	}
	return 0 // G_SOURCE_REMOVE
}

// norm maps a point on the picture to [0, 1] of the letterboxed video.
func (v *gtkView) norm(x, y float64) (float64, float64) {
	cw, ch := float64(g.widgetGetWidth(v.picture)), float64(g.widgetGetHeight(v.picture))
	v.mu.Lock()
	vw, vh := float64(v.w), float64(v.h)
	v.mu.Unlock()
	if vw <= 0 || vh <= 0 || cw <= 0 || ch <= 0 {
		return x / max(cw, 1), y / max(ch, 1)
	}
	s := min(cw/vw, ch/vh)
	w, h := vw*s, vh*s
	return (x - (cw-w)/2) / w, (y - (ch-h)/2) / h
}

func motionCallback(ctrl uintptr, x, y float64, id uintptr) {
	if v := views[id]; v != nil {
		v.move(v.norm(x, y))
	}
}

// gtkButton maps GDK's buttons (1 left, 2 middle, 3 right) to the browser's.
func gtkButton(b uint32) (int, bool) {
	switch b {
	case 1:
		return 0, true
	case 2:
		return 1, true
	case 3:
		return 2, true
	}
	return 0, false
}

func pressedCallback(gesture uintptr, n int32, x, y float64, id uintptr) {
	v := views[id]
	if v == nil {
		return
	}
	g.widgetGrabFocus(v.picture)
	if b, ok := gtkButton(g.singleGetButton(gesture)); ok {
		nx, ny := v.norm(x, y)
		v.button(b, true, nx, ny)
	}
}

func releasedCallback(gesture uintptr, n int32, x, y float64, id uintptr) {
	v := views[id]
	if v == nil {
		return
	}
	if b, ok := gtkButton(g.singleGetButton(gesture)); ok {
		nx, ny := v.norm(x, y)
		v.button(b, false, nx, ny)
	}
}

func scrollCallback(ctrl uintptr, dx, dy float64, id uintptr) int32 {
	v := views[id]
	if v == nil {
		return 0
	}
	if g.scrollGetUnit == nil || g.scrollGetUnit(ctrl) == gdkScrollUnitWheel {
		dx, dy = dx*wheelPixels, dy*wheelPixels
	}
	v.wheel(dx, dy)
	return 1
}

func keyDownCallback(ctrl uintptr, keyval, keycode, state uint32, id uintptr) int32 {
	v := views[id]
	if v == nil || keycode < 8 {
		return 0
	}
	code := keymap.FromEvdev(uint16(keycode - 8))
	if code == "" || v.in() == nil {
		return 0
	}
	v.key(code, true)
	return 1
}

func keyUpCallback(ctrl uintptr, keyval, keycode, state uint32, id uintptr) {
	if v := views[id]; v != nil && keycode >= 8 {
		v.key(keymap.FromEvdev(uint16(keycode-8)), false)
	}
}

func leaveCallback(ctrl uintptr, id uintptr) {
	if v := views[id]; v != nil {
		v.blur()
	}
}

func mapCallback(widget uintptr, id uintptr) {
	if v := views[id]; v != nil {
		g.widgetGrabFocus(v.picture)
	}
}

// clickedCallback opens the session menu under the button, in the
// coordinates of NSGo's root view: the surface in the window's box.
func clickedCallback(button uintptr, id uintptr) {
	v := views[id]
	if v == nil {
		return
	}
	root := g.widgetGetRoot(v.ball)
	surface := v.ball
	for surface != 0 {
		parent := g.widgetGetParent(surface)
		if parent != 0 && g.widgetGetParent(parent) == root {
			break
		}
		surface = parent
	}
	if surface == 0 {
		surface = root
	}
	var x, y float64
	if g.widgetTranslate(v.ball, surface, 0, float64(controlball.Size)+4, &x, &y) == 0 {
		return
	}
	v.blur()
	v.cx.Emit(ControlsEvent, core.Point{X: core.Points(x), Y: core.Points(y)})
}
