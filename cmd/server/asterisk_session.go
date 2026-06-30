package main

import (
	"context"
	"strings"

	"wacalls/internal/voip/call"
	"wacalls/internal/voip/core"

	"go.mau.fi/whatsmeow/types"
)

func (s *Session) bridgeIncomingToAsterisk(c *call.CallInfo) {
	if s.mgr.asterisk == nil {
		return
	}
	go s.bridgeIncomingToAsteriskAsync(c)
}

func (s *Session) bridgeIncomingToAsteriskAsync(c *call.CallInfo) {
	ac, ok := s.reg.get(c.CallID)
	if !ok {
		return
	}
	caller := s.sipCallerUser(context.Background(), c)
	called := sipUserFromOwnJID(s.client.Store.ID)
	callRoute, ok, err := s.mgr.asterisk.configForCall(context.Background(), s.id, called)
	if err != nil {
		s.log.Error("asterisk route lookup failed", "call_id", c.CallID, "err", err)
		return
	}
	if !ok {
		s.log.Debug("no asterisk route for inbound call", "call_id", c.CallID, "session", s.id, "wa_number", called)
		return
	}
	if callRoute.FromUser != "" {
		caller = callRoute.FromUser
	}
	if callRoute.ToUser != "" {
		called = callRoute.ToUser
	}
	leg, err := NewSIPLeg(callRoute.SIPConfig, c.CallID, caller, called, s.log)
	if err != nil {
		if callRoute.Release != nil {
			callRoute.Release()
		}
		s.log.Error("asterisk leg setup failed", "call_id", c.CallID, "err", err)
		_ = ac.cm.RejectCall(context.Background(), c.CallID, core.EndCallReasonDeclined)
		s.removeCall(c.CallID)
		s.mgr.broker.endCall(c.CallID, string(core.EndCallReasonDeclined))
		return
	}
	leg.OnReleased = callRoute.Release
	old, found := s.reg.setLeg(c.CallID, leg)
	if !found {
		leg.Close()
		return
	}
	if old != nil {
		old.Close()
	}
	owner := "asterisk"
	_ = s.mgr.broker.setOwner(c.CallID, owner)

	leg.OnPCM = func(pcm []float32) {
		ac.cm.FeedCapturedPCM(pcm)
	}
	leg.OnAnswered = func() {
		s.mgr.broker.emitIncomingClaimed(s.id, c.CallID, owner)
		if err := ac.cm.AcceptCall(context.Background(), c.CallID); err != nil {
			s.log.Error("accept WhatsApp call after SIP answer failed", "call_id", c.CallID, "err", err)
			leg.Close()
		}
	}
	leg.OnFailed = func(reason string) {
		s.log.Info("asterisk leg failed", "call_id", c.CallID, "reason", reason)
		_ = ac.cm.RejectCall(context.Background(), c.CallID, core.EndCallReasonDeclined)
		s.removeCall(c.CallID)
		s.mgr.broker.endCall(c.CallID, string(core.EndCallReasonDeclined))
	}
	leg.OnClosed = func() {
		s.log.Info("asterisk BYE received, ending WhatsApp call", "call_id", c.CallID)
		_ = ac.cm.EndCall(context.Background(), core.EndCallReasonUserEnded)
	}
	leg.Start()
}

func (s *Session) sipCallerUser(ctx context.Context, c *call.CallInfo) string {
	if c.CallerPn != "" {
		return sipUserFromJIDString(c.CallerPn)
	}
	if c.PeerJid != "" {
		if pn := s.resolvePNForSIPCaller(ctx, c.PeerJid); pn != "" {
			return pn
		}
		return sipUserFromJIDString(c.PeerJid)
	}
	return "unknown"
}

func (s *Session) resolvePNForSIPCaller(ctx context.Context, rawJID string) string {
	if s.client == nil || s.client.Store == nil || s.client.Store.LIDs == nil {
		return ""
	}
	jid, err := types.ParseJID(rawJID)
	if err != nil || jid.Server != types.HiddenUserServer {
		return ""
	}
	pn, err := s.client.Store.LIDs.GetPNForLID(ctx, jid.ToNonAD())
	if err != nil || pn.IsEmpty() {
		s.log.Debug("no PN mapping for LID caller", "lid", rawJID, "err", err)
		return ""
	}
	s.log.Info("resolved LID caller to PN", "lid", rawJID, "pn", pn.String())
	return sipUserFromJIDString(pn.String())
}

func sipUserFromOwnJID(jid *types.JID) string {
	if jid == nil {
		return "unknown"
	}
	return sipUserFromJIDString(jid.String())
}

func sipUserFromJIDString(raw string) string {
	user := raw
	if before, _, ok := strings.Cut(user, "@"); ok {
		user = before
	}
	if before, _, ok := strings.Cut(user, ":"); ok {
		user = before
	}
	digits := normalizePhone(user)
	if digits != "" {
		return digits
	}
	user = strings.TrimSpace(user)
	if user == "" {
		return "unknown"
	}
	return sanitizeSIPUser(user)
}
