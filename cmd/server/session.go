package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"wacalls/internal/voip/call"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/signaling"
	"wacalls/internal/voip/wanode"
	"wacalls/internal/wa"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type Session struct {
	id   string
	name string
	mgr  *SessionManager
	log  *slog.Logger

	client *whatsmeow.Client
	reg    *callRegistry

	mu   sync.Mutex
	auth AuthSnapshot
}

func newSession(mgr *SessionManager, id, name string, client *whatsmeow.Client) *Session {
	s := &Session{
		id:     id,
		name:   name,
		mgr:    mgr,
		log:    mgr.log.With("session", id),
		client: client,
		auth:   AuthSnapshot{State: "connecting"},
		reg:    newCallRegistry(),
	}
	client.AddEventHandler(s.handleEvent)
	return s
}

func (s *Session) createCall(callID string) *call.CallManager {
	cm := call.NewCallManager(wa.NewSocket(s.client), s.log)
	s.wireCall(cm, callID)
	s.reg.add(callID, &activeCall{cm: cm})
	return cm
}

func (s *Session) wireCall(cm *call.CallManager, callID string) {
	cm.OnIncoming = func(c *call.CallInfo) {
		s.mgr.broker.upsertCall(CallRecord{
			SessionID: s.id, CallID: c.CallID, Direction: "inbound", Peer: c.PeerJid,
			StartedAt: time.Now().UnixMilli(), Status: StatusRinging,
		})
		s.mgr.broker.emitIncoming(s.id, c.CallID, c.PeerJid)
		if s.mgr.asterisk.Enabled() {
			go s.bridgeIncomingToAsterisk(c)
		}
	}
	cm.OnStateChange = func(c *call.CallInfo) {
		if c.IsEnded() {
			s.removeCall(c.CallID)
			s.mgr.broker.endCall(c.CallID, string(c.StateData.EndReason))
			return
		}
		dir := "outbound"
		if c.Direction == core.CallDirectionIncoming {
			dir = "inbound"
		}
		existing, _ := s.mgr.broker.getCall(c.CallID)
		rec := CallRecord{
			SessionID: s.id, CallID: c.CallID, Direction: dir, Peer: c.PeerJid,
			StartedAt: time.Now().UnixMilli(), Status: mapStatus(c.StateData.State),
		}
		if existing != nil {
			rec.Owner = existing.Owner
			rec.StartedAt = existing.StartedAt
		}
		s.mgr.broker.upsertCall(rec)
	}
	cm.OnEnded = func(c *call.CallInfo) {
		s.log.Info("WhatsApp call ended, closing local media leg", "call_id", c.CallID, "reason", string(c.StateData.EndReason))
		s.removeCall(c.CallID)
		s.mgr.broker.endCall(c.CallID, string(c.StateData.EndReason))
	}
	cm.OnPeerAudio = func(pcm16 []float32) {
		ac, ok := s.reg.get(callID)
		if !ok || ac.leg == nil {
			return
		}
		_ = ac.leg.WritePCM(pcm16)
	}
}

func (s *Session) bridgeIncomingToAsterisk(c *call.CallInfo) {
	ac, ok := s.reg.get(c.CallID)
	if !ok {
		return
	}
	caller := s.sipCallerUser(context.Background(), c)
	called := sipUserFromOwnJID(s.client.Store.ID)
	leg, err := NewSIPLeg(s.mgr.asterisk, c.CallID, caller, called, s.log)
	if err != nil {
		s.log.Error("asterisk leg setup failed", "call_id", c.CallID, "err", err)
		_ = ac.cm.RejectCall(context.Background(), c.CallID, core.EndCallReasonDeclined)
		s.removeCall(c.CallID)
		s.mgr.broker.endCall(c.CallID, string(core.EndCallReasonDeclined))
		return
	}
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

func (s *Session) startOutgoing(ctx context.Context, peer types.JID, isVideo bool) (string, error) {
	callID := signaling.GenerateCallID()
	cm := s.createCall(callID)
	if err := cm.StartCall(ctx, callID, peer, isVideo); err != nil {
		s.removeCall(callID)
		return "", err
	}
	return callID, nil
}

func (s *Session) callForEvent(from types.JID, data *waBinary.Node) (*activeCall, bool) {
	callID := callIDFromNode(wrapCall(from, data))
	if callID == "" {
		return nil, false
	}
	return s.reg.get(callID)
}

func (s *Session) onIncomingOffer(ctx context.Context, evt *events.CallOffer) {
	node := wrapCall(evt.From, evt.Data)
	callID := callIDFromNode(node)
	if callID == "" {
		return
	}
	if max := s.mgr.maxCalls; max > 0 && s.reg.count() >= max {
		s.rejectOffer(ctx, node, evt.From)
		return
	}
	cm := s.createCall(callID)
	cm.HandleCallOffer(ctx, node, evt.From)
}

func (s *Session) rejectOffer(ctx context.Context, node *waBinary.Node, from types.JID) {
	info := signaling.ExtractNodeInfo(node)
	if info == nil {
		return
	}
	creator := wanode.AttrString(info.InnerNode.Attrs, "call-creator")
	if creator == "" {
		creator = from.String()
	}
	reject := signaling.BuildRejectStanza(from, info.CallID, wanode.MustJID(creator))
	_ = wa.NewSocket(s.client).SendNode(ctx, reject)
	s.log.Info("inbound call rejected: session at capacity", "call_id", info.CallID)
}

func (s *Session) handleEvent(rawEvt any) {
	ctx := context.Background()
	switch evt := rawEvt.(type) {
	case *events.Connected:
		if id := s.client.Store.ID; id != nil {
			_ = s.mgr.store.setJID(s.mgr.appCtx, s.id, id.String())
		}
		s.setAuth(AuthSnapshot{State: "open", Paired: true})
	case *events.LoggedOut:
		s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
	case *events.CallOffer:
		s.onIncomingOffer(ctx, evt)
	case *events.CallAccept:
		if ac, ok := s.callForEvent(evt.From, evt.Data); ok {
			ac.cm.HandleCallAccept(ctx, wrapCall(evt.From, evt.Data), evt.From)
		}
	case *events.CallTransport:
		if ac, ok := s.callForEvent(evt.From, evt.Data); ok {
			ac.cm.HandleCallTransport(ctx, wrapCall(evt.From, evt.Data), evt.From)
		}
	case *events.CallTerminate:
		if ac, ok := s.callForEvent(evt.From, evt.Data); ok {
			ac.cm.HandleCallTerminate(wrapCall(evt.From, evt.Data))
		}
	case *events.CallReject:
		if ac, ok := s.callForEvent(evt.From, evt.Data); ok {
			ac.cm.HandleCallTerminate(wrapCall(evt.From, evt.Data))
		}
	}
}

func (s *Session) connect(ctx context.Context) error {
	if s.client.Store.ID != nil {
		return s.client.Connect()
	}
	return s.startPairing(ctx)
}

func (s *Session) startPairing(ctx context.Context) error {
	qrChan, err := s.client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := s.client.Connect(); err != nil {
		return err
	}
	go func() {
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				s.log.Info("scan the QR code to pair this session")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				s.setAuth(AuthSnapshot{State: "qr", QR: evt.Code})
				s.mgr.broker.emitSessionQR(s.id, evt.Code)
			case "success":
				if id := s.client.Store.ID; id != nil {
					_ = s.mgr.store.setJID(s.mgr.appCtx, s.id, id.String())
				}
				s.setAuth(AuthSnapshot{State: "open", Paired: true})
			case "timeout":
				s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
			}
		}
	}()
	return nil
}

func (s *Session) setAuth(a AuthSnapshot) {
	s.mu.Lock()
	s.auth = a
	s.mu.Unlock()
	s.mgr.broker.emitAuthState(s.id, a)
	s.mgr.broker.emitSessionList(s.mgr.infos())
}

func (s *Session) info() SessionInfo {
	s.mu.Lock()
	a := s.auth
	s.mu.Unlock()
	jid := ""
	if id := s.client.Store.ID; id != nil {
		jid = id.String()
	}
	return SessionInfo{ID: s.id, Name: s.name, JID: jid, State: a.State, Paired: a.Paired || jid != ""}
}

func (s *Session) setBridge(callID string, b *Bridge) {
	oldB, found := s.reg.setLeg(callID, b)
	if !found {
		b.Close()
		return
	}
	if oldB != nil {
		oldB.Close()
	}
}

func (s *Session) removeCall(callID string) {
	ac, ok := s.reg.remove(callID)
	if !ok {
		return
	}
	if ac.leg != nil {
		ac.leg.Close()
	}
}

func (s *Session) terminateCall(callID string, reason core.EndCallReason) {
	ac, ok := s.reg.get(callID)
	if !ok {
		return
	}
	_ = ac.cm.EndCall(context.Background(), reason)
}

func (s *Session) teardownAllCalls() {
	for _, ac := range s.reg.drain() {
		_ = ac.cm.EndCall(context.Background(), core.EndCallReasonUserEnded)
		if ac.leg != nil {
			ac.leg.Close()
		}
	}
}

func (s *Session) replaceClient(client *whatsmeow.Client) {
	s.teardownAllCalls()
	s.client.Disconnect()
	s.client = client
	client.AddEventHandler(s.handleEvent)
}

func (s *Session) shutdown() {
	s.teardownAllCalls()
	s.client.Disconnect()
}

func mapStatus(state core.CallState) CallStatus {
	switch state {
	case core.CallStateActive:
		return StatusConnected
	case core.CallStateEnded:
		return StatusEnded
	case core.CallStateInitiating:
		return StatusStarting
	default:
		return StatusRinging
	}
}
