package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wacalls/internal/voip/core"

	"go.mau.fi/whatsmeow/types"
)

type AsteriskSIPServer struct {
	gateway *AsteriskGateway
	manager *SessionManager
	log     *slog.Logger
	conn    *net.UDPConn
	localIP string

	mu      sync.Mutex
	dialogs map[string]*SIPInboundLeg
}

func (g *AsteriskGateway) StartSIPServer(ctx context.Context, manager *SessionManager, log *slog.Logger) (*AsteriskSIPServer, error) {
	bind := strings.TrimSpace(g.defaults.ListenBind)
	if bind == "" || bind == ":0" {
		return nil, nil
	}
	conn, err := net.ListenUDP("udp", mustUDPAddr(bind))
	if err != nil {
		return nil, fmt.Errorf("listen asterisk sip: %w", err)
	}
	localIP := g.defaults.AdvertiseIP
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	s := &AsteriskSIPServer{
		gateway: g,
		manager: manager,
		log:     log,
		conn:    conn,
		localIP: localIP,
		dialogs: map[string]*SIPInboundLeg{},
	}
	go s.loop(ctx)
	log.Info("Asterisk SIP server listening", "addr", conn.LocalAddr().String())
	return s, nil
}

func (s *AsteriskSIPServer) Close() {
	if s == nil || s.conn == nil {
		return
	}
	_ = s.conn.Close()
}

func (s *AsteriskSIPServer) loop(ctx context.Context) {
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	buf := make([]byte, 8192)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg := string(buf[:n])
		if strings.HasPrefix(msg, "INVITE ") {
			go s.handleInvite(context.Background(), msg, addr)
			continue
		}
		if strings.HasPrefix(msg, "OPTIONS ") {
			s.handleOptions(context.Background(), msg, addr)
			continue
		}
		key := sipDialogKey(msg)
		if key == "" {
			continue
		}
		s.mu.Lock()
		leg := s.dialogs[key]
		s.mu.Unlock()
		if leg != nil {
			leg.handleSIP(msg)
		}
	}
}

func (s *AsteriskSIPServer) handleInvite(ctx context.Context, msg string, addr *net.UDPAddr) {
	req, err := parseInboundInvite(msg, addr)
	if err != nil {
		s.log.Warn("bad asterisk INVITE", "err", err)
		sendStatelessSIPResponse(s.conn, addr, msg, 400, "Bad Request", "")
		return
	}
	if req.WANumber == "" {
		s.log.Warn("asterisk SIP INVITE rejected without X-WaCalls-Number", "remote_sip", addr.String(), "call_id", req.CallID, "target", req.TargetUser)
		sendStatelessSIPResponse(s.conn, addr, msg, 400, "X-WaCalls-Number Required", "")
		return
	}
	route, ok, err := s.gateway.outboundRouteForSource(ctx, req.WANumber, addr.IP)
	if err != nil {
		s.log.Warn("asterisk route lookup failed", "remote_sip", addr.String(), "wa_number", req.WANumber, "err", err)
		sendStatelessSIPResponse(s.conn, addr, msg, 500, "Route Lookup Failed", "")
		return
	}
	if !ok {
		s.log.Warn("asterisk SIP INVITE rejected for unregistered source/number", "remote_sip", addr.String(), "call_id", req.CallID, "wa_number", req.WANumber, "target", req.TargetUser)
		sendStatelessSIPResponse(s.conn, addr, msg, 403, "Forbidden", "")
		return
	}
	sess, err := s.selectOutboundSession(req, route)
	if err != nil {
		s.log.Warn("no WhatsApp session for outbound SIP call", "call_id", req.CallID, "wa_number", req.WANumber, "target", req.TargetUser, "err", err)
		sendStatelessSIPResponse(s.conn, addr, msg, 404, "No WhatsApp Session", "")
		return
	}
	s.log.Info("asterisk SIP INVITE received", "remote_sip", addr.String(), "call_id", req.CallID, "wa_number", req.WANumber, "target", req.TargetUser, "from", req.FromUser)
	sendStatelessSIPResponse(s.conn, addr, msg, 100, "Trying", "")

	rtpBind, release, err := s.gateway.acquireRTPBind()
	if err != nil {
		s.log.Error("no RTP port for outbound SIP call", "call_id", req.CallID, "err", err)
		sendStatelessSIPResponse(s.conn, addr, msg, 503, "No RTP Port", "")
		return
	}
	leg, err := NewSIPInboundLeg(SIPInboundConfig{
		Request:     req,
		Conn:        s.conn,
		RTPBind:     rtpBind,
		AdvertiseIP: s.gateway.defaults.AdvertiseIP,
		LocalIP:     s.localIP,
		Log:         sess.log,
	})
	if err != nil {
		if release != nil {
			release()
		}
		sendStatelessSIPResponse(s.conn, addr, msg, 488, "Media Error", "")
		return
	}
	leg.OnReleased = release
	s.setDialog(req.CallID, leg)
	leg.OnDone = func() { s.deleteDialog(req.CallID) }

	peer := types.NewJID(normalizePhone(req.TargetUser), types.DefaultUserServer)
	callID, err := sess.startOutgoing(ctx, peer, false)
	if err != nil {
		s.log.Error("start WhatsApp outbound call failed", "sip_call_id", req.CallID, "target", req.TargetUser, "err", err)
		leg.Reject(503, "WhatsApp Call Failed")
		return
	}
	leg.SetCallID(callID)
	ac, ok := sess.reg.get(callID)
	if !ok {
		leg.Reject(500, "Call Registry Error")
		return
	}
	old, found := sess.reg.setLeg(callID, leg)
	if !found {
		leg.Reject(500, "Call Registry Error")
		return
	}
	if old != nil {
		old.Close()
	}
	leg.OnPCM = func(pcm []float32) {
		ac.cm.FeedCapturedPCM(pcm)
	}
	leg.OnClosed = func() {
		_ = ac.cm.EndCall(context.Background(), core.EndCallReasonUserEnded)
	}
	leg.Start()
	leg.Ring()
	s.log.Info("outbound WhatsApp call started from Asterisk", "session", sess.id, "wa_number", req.WANumber, "remote_sip", addr.String(), "sip_call_id", req.CallID, "call_id", callID, "target", req.TargetUser)
}

func (s *AsteriskSIPServer) handleOptions(ctx context.Context, req string, addr *net.UDPAddr) {
	allowed, err := s.gateway.sourceAllowed(ctx, addr.IP)
	if err != nil {
		s.log.Warn("asterisk OPTIONS source check failed", "remote_sip", addr.String(), "err", err)
		sendStatelessSIPResponse(s.conn, addr, req, 500, "Source Check Failed", "")
		return
	}
	if !allowed {
		s.log.Warn("asterisk OPTIONS rejected from unknown source", "remote_sip", addr.String())
		sendStatelessSIPResponse(s.conn, addr, req, 403, "Forbidden", "")
		return
	}
	headers := sipHeaders(req)
	var b strings.Builder
	b.WriteString("SIP/2.0 200 OK\r\n")
	if via := headers["via"]; via != "" {
		b.WriteString("Via: " + via + "\r\n")
	}
	if from := headers["from"]; from != "" {
		b.WriteString("From: " + from + "\r\n")
	}
	if to := headers["to"]; to != "" {
		if !strings.Contains(strings.ToLower(to), ";tag=") {
			to += ";tag=" + token(8)
		}
		b.WriteString("To: " + to + "\r\n")
	}
	if callID := headers["call-id"]; callID != "" {
		b.WriteString("Call-ID: " + callID + "\r\n")
	}
	if cseq := headers["cseq"]; cseq != "" {
		b.WriteString("CSeq: " + cseq + "\r\n")
	}
	b.WriteString("Contact: <sip:wacalls@" + s.localIP + ":" + strconv.Itoa(s.conn.LocalAddr().(*net.UDPAddr).Port) + ">\r\n")
	b.WriteString("Allow: INVITE, ACK, CANCEL, BYE, OPTIONS\r\n")
	b.WriteString("Accept: application/sdp\r\n")
	b.WriteString("User-Agent: WaCalls-Asterisk-Gateway\r\n")
	b.WriteString("Content-Length: 0\r\n\r\n")
	_, _ = s.conn.WriteToUDP([]byte(b.String()), addr)
	s.log.Debug("asterisk OPTIONS answered", "remote", addr.String())
}

func (s *AsteriskSIPServer) setDialog(callID string, leg *SIPInboundLeg) {
	s.mu.Lock()
	s.dialogs[callID] = leg
	s.mu.Unlock()
}

func (s *AsteriskSIPServer) deleteDialog(callID string) {
	s.mu.Lock()
	delete(s.dialogs, callID)
	s.mu.Unlock()
}

func (s *AsteriskSIPServer) selectOutboundSession(req inboundSIPInvite, route AsteriskRoute) (*Session, error) {
	if route.SessionID == "" {
		return nil, fmt.Errorf("route has no session")
	}
	sess, ok := s.manager.Get(route.SessionID)
	if !ok || !sess.info().Paired {
		return nil, fmt.Errorf("route session %s not paired", route.SessionID)
	}
	if got := sipUserFromOwnJID(sess.client.Store.ID); got != req.WANumber {
		return nil, fmt.Errorf("route session number mismatch: route=%s session=%s", req.WANumber, got)
	}
	return sess, nil
}

type inboundSIPInvite struct {
	Raw        string
	RemoteSIP  *net.UDPAddr
	RemoteRTP  *net.UDPAddr
	TargetUser string
	FromUser   string
	SessionID  string
	WANumber   string
	CallID     string
	ToTag      string
}

func parseInboundInvite(msg string, addr *net.UDPAddr) (inboundSIPInvite, error) {
	line, _, _ := strings.Cut(msg, "\r\n")
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return inboundSIPInvite{}, fmt.Errorf("missing request URI")
	}
	user := sipURIUser(fields[1])
	if normalizePhone(user) == "" {
		return inboundSIPInvite{}, fmt.Errorf("request URI user must be WhatsApp number")
	}
	headers := sipHeaders(msg)
	rtp, err := parseSDPRTP(msg, addr.IP.String())
	if err != nil {
		return inboundSIPInvite{}, err
	}
	return inboundSIPInvite{
		Raw:        msg,
		RemoteSIP:  addr,
		RemoteRTP:  rtp,
		TargetUser: normalizePhone(user),
		FromUser:   sanitizeSIPUser(sipURIUser(headers["from"])),
		SessionID:  strings.TrimSpace(headers["x-wacalls-session"]),
		WANumber:   normalizePhone(headers["x-wacalls-number"]),
		CallID:     headers["call-id"],
		ToTag:      token(8),
	}, nil
}

type SIPInboundConfig struct {
	Request     inboundSIPInvite
	Conn        *net.UDPConn
	RTPBind     string
	AdvertiseIP string
	LocalIP     string
	Log         *slog.Logger
}

type SIPInboundLeg struct {
	req       inboundSIPInvite
	conn      *net.UDPConn
	rtpConn   *net.UDPConn
	localIP   string
	rtpPort   int
	callID    string
	sipCallID string
	log       *slog.Logger
	ssrc      uint32
	txSeq     uint16
	txTS      uint32
	txQueue   chan []byte
	answered  atomic.Bool
	closed    atomic.Bool

	OnPCM      func([]float32)
	OnClosed   func()
	OnReleased func()
	OnDone     func()
}

func NewSIPInboundLeg(cfg SIPInboundConfig) (*SIPInboundLeg, error) {
	rtpConn, err := net.ListenUDP("udp", mustUDPAddr(cfg.RTPBind))
	if err != nil {
		return nil, fmt.Errorf("listen inbound rtp: %w", err)
	}
	localIP := cfg.AdvertiseIP
	if localIP == "" {
		localIP = cfg.LocalIP
	}
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	return &SIPInboundLeg{
		req:       cfg.Request,
		conn:      cfg.Conn,
		rtpConn:   rtpConn,
		localIP:   localIP,
		rtpPort:   rtpConn.LocalAddr().(*net.UDPAddr).Port,
		sipCallID: cfg.Request.CallID,
		log:       cfg.Log,
		ssrc:      randUint32(),
		txSeq:     uint16(randUint32()),
		txTS:      randUint32(),
		txQueue:   make(chan []byte, 64),
	}, nil
}

func (l *SIPInboundLeg) SetCallID(callID string) {
	l.callID = callID
}

func (l *SIPInboundLeg) Start() {
	go l.rtpLoop()
	go l.rtpTxLoop()
}

func (l *SIPInboundLeg) Ring() {
	l.sendResponse(180, "Ringing", "")
}

func (l *SIPInboundLeg) Answer() {
	if l.closed.Load() || l.answered.Swap(true) {
		return
	}
	l.sendResponse(200, "OK", l.localSDP())
	l.log.Info("asterisk outbound SIP leg answered", "call_id", l.callID, "sip_call_id", l.sipCallID, "rtp", l.req.RemoteRTP.String())
}

func (l *SIPInboundLeg) Reject(code int, text string) {
	if l.closed.Swap(true) {
		return
	}
	l.sendResponse(code, text, "")
	l.log.Info("asterisk outbound SIP leg rejected", "call_id", l.callID, "sip_call_id", l.sipCallID, "code", code, "reason", text)
	l.cleanup()
}

func (l *SIPInboundLeg) WritePCM(pcm16 []float32) error {
	if !l.answered.Load() || len(pcm16) == 0 {
		return nil
	}
	pcm8 := downsample16To8(pcm16)
	for len(pcm8) >= 160 {
		payload := make([]byte, 160)
		for i := range payload {
			payload[i] = linearToULaw(floatToInt16(pcm8[i]))
		}
		select {
		case l.txQueue <- payload:
		default:
			return nil
		}
		pcm8 = pcm8[160:]
	}
	return nil
}

func (l *SIPInboundLeg) Close() {
	if l.closed.Swap(true) {
		return
	}
	if l.answered.Load() {
		l.sendRequest("BYE")
		l.log.Info("asterisk outbound SIP BYE sent", "call_id", l.callID, "sip_call_id", l.sipCallID, "target", l.req.RemoteSIP.String())
	} else {
		l.sendResponse(480, "Temporarily Unavailable", "")
		l.log.Info("asterisk outbound SIP leg closed before answer", "call_id", l.callID, "sip_call_id", l.sipCallID)
	}
	l.cleanup()
}

func (l *SIPInboundLeg) handleSIP(msg string) {
	switch {
	case strings.HasPrefix(msg, "ACK "):
		return
	case strings.HasPrefix(msg, "CANCEL "):
		l.log.Info("asterisk outbound SIP CANCEL received", "call_id", l.callID, "sip_call_id", l.sipCallID)
		l.sendSIPResponseFor(msg, 200, "OK")
		l.sendResponse(487, "Request Terminated", "")
		l.closeRemote()
	case strings.HasPrefix(msg, "BYE "):
		l.log.Info("asterisk outbound SIP BYE received", "call_id", l.callID, "sip_call_id", l.sipCallID)
		l.sendSIPResponseFor(msg, 200, "OK")
		l.closeRemote()
	}
}

func (l *SIPInboundLeg) closeRemote() {
	if l.closed.Swap(true) {
		return
	}
	l.cleanup()
	if l.OnClosed != nil {
		l.OnClosed()
	}
}

func (l *SIPInboundLeg) cleanup() {
	_ = l.rtpConn.Close()
	close(l.txQueue)
	if l.OnReleased != nil {
		l.OnReleased()
	}
	if l.OnDone != nil {
		l.OnDone()
	}
}

func (l *SIPInboundLeg) rtpLoop() {
	buf := make([]byte, 2048)
	for {
		n, _, err := l.rtpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 12 || buf[1]&0x7f != 0 {
			continue
		}
		payload := buf[12:n]
		pcm8 := make([]float32, len(payload))
		for i, b := range payload {
			pcm8[i] = float32(uLawToLinear(b)) / 32768.0
		}
		if l.OnPCM != nil {
			l.OnPCM(upsample8To16(pcm8))
		}
	}
}

func (l *SIPInboundLeg) rtpTxLoop() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for payload := range l.txQueue {
		<-ticker.C
		pkt := make([]byte, 12+len(payload))
		pkt[0] = 0x80
		pkt[1] = 0
		binary.BigEndian.PutUint16(pkt[2:], l.txSeq)
		binary.BigEndian.PutUint32(pkt[4:], l.txTS)
		binary.BigEndian.PutUint32(pkt[8:], l.ssrc)
		copy(pkt[12:], payload)
		l.txSeq++
		l.txTS += 160
		_, _ = l.rtpConn.WriteToUDP(pkt, l.req.RemoteRTP)
	}
}

func (l *SIPInboundLeg) sendResponse(code int, text, body string) {
	contentType := ""
	if body != "" {
		contentType = "application/sdp"
	}
	sendStatelessSIPResponse(l.conn, l.req.RemoteSIP, l.req.Raw, code, text, body, contentType, l.req.ToTag)
}

func (l *SIPInboundLeg) sendSIPResponseFor(req string, code int, text string) {
	sendStatelessSIPResponse(l.conn, l.req.RemoteSIP, req, code, text, "")
}

func (l *SIPInboundLeg) sendRequest(method string) {
	headers := sipHeaders(l.req.Raw)
	to := headers["to"]
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=" + l.req.ToTag
	}
	from := headers["from"]
	contactUser := sanitizeSIPUser(l.req.TargetUser)
	remoteTarget := headers["contact"]
	if remoteTarget == "" {
		remoteTarget = "<sip:" + l.req.FromUser + "@" + l.req.RemoteSIP.String() + ">"
	}
	uri := strings.Trim(remoteTarget, "<>")
	var b strings.Builder
	b.WriteString(method + " " + uri + " SIP/2.0\r\n")
	b.WriteString("Via: SIP/2.0/UDP " + l.localIP + ":" + strconv.Itoa(l.conn.LocalAddr().(*net.UDPAddr).Port) + ";branch=z9hG4bK" + token(12) + "\r\n")
	b.WriteString("Max-Forwards: 70\r\n")
	b.WriteString("From: " + to + "\r\n")
	b.WriteString("To: " + from + "\r\n")
	b.WriteString("Call-ID: " + l.req.CallID + "\r\n")
	b.WriteString("CSeq: 2 " + method + "\r\n")
	b.WriteString("Contact: <sip:" + contactUser + "@" + l.localIP + ":" + strconv.Itoa(l.conn.LocalAddr().(*net.UDPAddr).Port) + ">\r\n")
	b.WriteString("User-Agent: WaCalls-Asterisk-Gateway\r\n")
	b.WriteString("Content-Length: 0\r\n\r\n")
	_, _ = l.conn.WriteToUDP([]byte(b.String()), l.req.RemoteSIP)
	l.log.Debug("asterisk outbound SIP request sent", "call_id", l.callID, "sip_call_id", l.sipCallID, "method", method, "target", uri)
}

func (l *SIPInboundLeg) localSDP() string {
	return "v=0\r\n" +
		"o=wacalls 0 0 IN IP4 " + l.localIP + "\r\n" +
		"s=WaCalls\r\n" +
		"c=IN IP4 " + l.localIP + "\r\n" +
		"t=0 0\r\n" +
		"m=audio " + strconv.Itoa(l.rtpPort) + " RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=sendrecv\r\n"
}

func sendStatelessSIPResponse(conn *net.UDPConn, addr *net.UDPAddr, req string, code int, text string, body string, extras ...string) {
	contentType := ""
	toTag := ""
	if len(extras) > 0 {
		contentType = extras[0]
	}
	if len(extras) > 1 {
		toTag = extras[1]
	}
	headers := sipHeaders(req)
	to := headers["to"]
	if toTag != "" && !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=" + toTag
	}
	var b strings.Builder
	b.WriteString("SIP/2.0 " + strconv.Itoa(code) + " " + text + "\r\n")
	if via := headers["via"]; via != "" {
		b.WriteString("Via: " + via + "\r\n")
	}
	if from := headers["from"]; from != "" {
		b.WriteString("From: " + from + "\r\n")
	}
	if to != "" {
		b.WriteString("To: " + to + "\r\n")
	}
	if callID := headers["call-id"]; callID != "" {
		b.WriteString("Call-ID: " + callID + "\r\n")
	}
	if cseq := headers["cseq"]; cseq != "" {
		b.WriteString("CSeq: " + cseq + "\r\n")
	}
	b.WriteString("User-Agent: WaCalls-Asterisk-Gateway\r\n")
	if contentType != "" {
		b.WriteString("Content-Type: " + contentType + "\r\n")
	}
	b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
	b.WriteString(body)
	_, _ = conn.WriteToUDP([]byte(b.String()), addr)
}

func sipDialogKey(msg string) string {
	headers := sipHeaders(msg)
	return headers["call-id"]
}

func sipURIUser(raw string) string {
	raw = strings.TrimSpace(raw)
	if start := strings.Index(raw, "sip:"); start >= 0 {
		raw = raw[start+4:]
	}
	raw = strings.Trim(raw, "<>")
	if at := strings.Index(raw, "@"); at >= 0 {
		raw = raw[:at]
	}
	if semi := strings.Index(raw, ";"); semi >= 0 {
		raw = raw[:semi]
	}
	return raw
}
