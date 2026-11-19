package client

// While rich sharing is on, route paste through the clipboard poller. This
// orders the V keystroke after the remote has acknowledged the last chunk.
func (s *Session) StartPasteSync() <-chan uint64 {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	s.pasteRequests = make(chan uint64, 1)
	s.pasteSeq++
	return s.pasteRequests
}
func (s *Session) StopPasteSync() {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	s.pasteRequests = nil
	s.pasteSeq++
}

// Called with keyMu held, before forwarding a key down.
func (s *Session) requestPaste(code string) bool {
	if code != "KeyV" || s.pasteRequests == nil || len(s.down) != 1 {
		return false
	}
	for mod := range s.down {
		switch mod {
		case "ControlLeft", "ControlRight", "MetaLeft", "MetaRight":
			s.pasteMod = mod
		default:
			return false
		}
	}
	select {
	case <-s.pasteRequests:
	default:
	}
	s.pasteRequests <- s.pasteSeq
	return true
}
func (s *Session) FinishPaste(seq uint64) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.pasteRequests == nil || s.pasteSeq != seq {
		return
	}
	mod := s.pasteMod
	held := s.down[mod]
	if !held {
		s.send(msg{T: "kd", C: mod})
	}
	s.send(msg{T: "kd", C: "KeyV"})
	s.send(msg{T: "ku", C: "KeyV"})
	if !held {
		s.send(msg{T: "ku", C: mod})
	}
	s.pasteSeq++
}
func (s *Session) cancelPaste() {
	s.keyMu.Lock()
	s.pasteSeq++
	s.keyMu.Unlock()
}
