package main

import (
	"go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func (s *Session) handleCallEndEvent(event string, from types.JID, data *binary.Node) {
	node := wrapCall(from, data)
	callID := callIDFromNode(node)
	if callID != "" {
		if ac, ok := s.reg.get(callID); ok {
			ac.cm.HandleCallTerminate(node)
			return
		}
	}
	activeID, ac, ok, count := s.reg.only()
	if !ok {
		s.log.Warn("WhatsApp call end event did not match active call", "event", event, "from", from.String(), "event_call_id", callID, "active_calls", count)
		return
	}
	s.log.Warn("WhatsApp call end event matched by single active call fallback", "event", event, "from", from.String(), "event_call_id", callID, "active_call_id", activeID)
	ac.cm.HandleCallTerminate(node)
}
