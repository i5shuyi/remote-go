package client

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestKeyboardFocusAndChords(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	received := make(chan []string, 1)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		var events []string
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m msg
			if json.Unmarshal(data, &m) != nil {
				return
			}
			switch m.T {
			case "kd", "ku":
				events = append(events, m.T+" "+m.C)
			case "tx": // end of the ordered keyboard sequence
				received <- events
				return
			}
		}
	}))
	defer host.Close()
	var dialer net.Dialer
	s, err := Dial(ctx, dialer.DialContext, strings.TrimPrefix(host.URL, "http://"), "test", false, Config{}, Handler{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Key("ControlLeft", true)
	s.SendKeys("ControlLeft", "AltLeft", "Delete")
	s.ReleaseKeys()
	s.Key("ControlLeft", false) // physical release after returning to the window
	s.Key("KeyA", true)
	s.Key("KeyA", true) // auto-repeat
	s.Key("KeyA", false)
	requests := s.StartPasteSync()
	s.SendKeys("MetaLeft", "KeyV")
	seq := <-requests
	// The key must arrive after the clipboard acknowledgement even if
	// the physical modifier has already been released.
	s.FinishPaste(seq)
	s.SendKeys("ControlLeft", "KeyV")
	seq = <-requests
	s.ReleaseKeys()
	s.FinishPaste(seq) // focus changed while transferring
	s.StopPasteSync()
	s.Text("done")

	want := []string{"kd ControlLeft", "kd AltLeft", "kd Delete", "ku Delete", "ku AltLeft", "ku ControlLeft", "kd KeyA", "kd KeyA", "ku KeyA", "kd MetaLeft", "ku MetaLeft", "kd MetaLeft", "kd KeyV", "ku KeyV", "ku MetaLeft", "kd ControlLeft", "ku ControlLeft"}
	select {
	case got := <-received:
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("keyboard events %v, want %v", got, want)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
