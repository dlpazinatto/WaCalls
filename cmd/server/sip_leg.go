package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type SIPConfig struct {
	Target      string
	FromUser    string
	Bind        string
	AdvertiseIP string
}

func (c SIPConfig) Enabled() bool {
	return strings.TrimSpace(c.Target) != ""
}

type SIPLeg struct {
	cfg       SIPConfig
	callID    string
	peer      string
	targetURI string
	fromURI   string
	contact   string
	remoteSIP *net.UDPAddr
	sipConn   *net.UDPConn
	rtpConn   *net.UDPConn
	localIP   string
	sipPort   int
	rtpPort   int
	fromTag   string
	toTag     string
	branch    string
	sipID     string
	ssrc      uint32
	log       *slog.Logger

	mu        sync.RWMutex
	remoteRTP *net.UDPAddr
	answered  bool
	closed    atomic.Bool
	txSeq     uint16
	txTS      uint32
	txQueue   chan []byte

	OnAnswered func()
	OnFailed   func(reason string)
	OnClosed   func()
	OnPCM      func([]float32)
}

func NewSIPLeg(cfg SIPConfig, callID, peer string, log *slog.Logger) (*SIPLeg, error) {
	targetURI, remoteSIP, err := parseSIPTarget(cfg.Target)
	if err != nil {
		return nil, err
	}
	if cfg.FromUser == "" {
		cfg.FromUser = "wacalls"
	}
	if cfg.Bind == "" {
		cfg.Bind = ":0"
	}
	sipConn, err := net.ListenUDP("udp", mustUDPAddr(cfg.Bind))
	if err != nil {
		return nil, fmt.Errorf("listen sip: %w", err)
	}
	rtpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		_ = sipConn.Close()
		return nil, fmt.Errorf("listen rtp: %w", err)
	}
	localIP := cfg.AdvertiseIP
	if localIP == "" {
		localIP = outboundIP(remoteSIP)
	}
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	rtpPort := rtpConn.LocalAddr().(*net.UDPAddr).Port
	sipPort := sipConn.LocalAddr().(*net.UDPAddr).Port
	fromURI := "sip:" + cfg.FromUser + "@" + localIP
	leg := &SIPLeg{
		cfg:       cfg,
		callID:    callID,
		peer:      peer,
		targetURI: targetURI,
		fromURI:   fromURI,
		contact:   "<" + fromURI + ":" + strconv.Itoa(sipPort) + ">",
		remoteSIP: remoteSIP,
		sipConn:   sipConn,
		rtpConn:   rtpConn,
		localIP:   localIP,
		sipPort:   sipPort,
		rtpPort:   rtpPort,
		fromTag:   token(8),
		branch:    "z9hG4bK" + token(12),
		sipID:     token(16) + "@" + localIP,
		ssrc:      randUint32(),
		txSeq:     uint16(randUint32()),
		txTS:      randUint32(),
		txQueue:   make(chan []byte, 64),
		log:       log,
	}
	return leg, nil
}

func (l *SIPLeg) Start() {
	go l.sipLoop()
	go l.rtpLoop()
	go l.rtpTxLoop()
	l.sendInvite()
}

func (l *SIPLeg) WritePCM(pcm16 []float32) error {
	l.mu.RLock()
	ready := l.remoteRTP != nil && l.answered
	l.mu.RUnlock()
	if !ready || len(pcm16) == 0 {
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

func (l *SIPLeg) Close() {
	l.close(true)
}

func (l *SIPLeg) close(sendHangup bool) {
	if l.closed.Swap(true) {
		return
	}
	if sendHangup {
		l.mu.RLock()
		answered := l.answered
		l.mu.RUnlock()
		if answered {
			l.sendBye()
		} else {
			l.sendCancel()
		}
	}
	_ = l.sipConn.Close()
	_ = l.rtpConn.Close()
	close(l.txQueue)
}

func (l *SIPLeg) sipLoop() {
	buf := make([]byte, 8192)
	for {
		n, _, err := l.sipConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg := string(buf[:n])
		if strings.HasPrefix(msg, "SIP/2.0 ") {
			l.handleResponse(msg)
			continue
		}
		if strings.HasPrefix(msg, "BYE ") {
			l.sendSIPResponse(msg, 200, "OK")
			l.close(false)
			if l.OnClosed != nil {
				l.OnClosed()
			}
		}
	}
}

func (l *SIPLeg) handleResponse(msg string) {
	code := sipStatusCode(msg)
	if code == 0 {
		return
	}
	headers := sipHeaders(msg)
	cseq := strings.ToUpper(headers["cseq"])
	if code >= 100 && code < 200 {
		return
	}
	if code >= 300 {
		if l.OnFailed != nil {
			l.OnFailed(fmt.Sprintf("sip_%d", code))
		}
		l.Close()
		return
	}
	if code != 200 || !strings.Contains(cseq, "INVITE") {
		return
	}
	if to := headers["to"]; to != "" {
		l.toTag = headerParam(to, "tag")
	}
	rtpAddr, err := parseSDPRTP(msg, l.remoteSIP.IP.String())
	if err != nil {
		if l.OnFailed != nil {
			l.OnFailed("bad_sdp")
		}
		l.Close()
		return
	}
	l.mu.Lock()
	l.remoteRTP = rtpAddr
	l.answered = true
	l.mu.Unlock()
	l.sendAck()
	l.log.Info("asterisk SIP leg answered", "call_id", l.callID, "rtp", rtpAddr.String())
	if l.OnAnswered != nil {
		l.OnAnswered()
	}
}

func (l *SIPLeg) rtpLoop() {
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

func (l *SIPLeg) rtpTxLoop() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for payload := range l.txQueue {
		<-ticker.C
		l.mu.RLock()
		remote := l.remoteRTP
		l.mu.RUnlock()
		if remote == nil {
			continue
		}
		pkt := make([]byte, 12+len(payload))
		pkt[0] = 0x80
		pkt[1] = 0
		binary.BigEndian.PutUint16(pkt[2:], l.txSeq)
		binary.BigEndian.PutUint32(pkt[4:], l.txTS)
		binary.BigEndian.PutUint32(pkt[8:], l.ssrc)
		copy(pkt[12:], payload)
		l.txSeq++
		l.txTS += 160
		_, _ = l.rtpConn.WriteToUDP(pkt, remote)
	}
}

func (l *SIPLeg) sendInvite() {
	sdp := l.localSDP()
	msg := l.baseRequest("INVITE", 1, sdp, "application/sdp")
	_, _ = l.sipConn.WriteToUDP([]byte(msg), l.remoteSIP)
	l.log.Info("asterisk SIP INVITE sent", "call_id", l.callID, "target", l.targetURI, "rtp_port", l.rtpPort)
}

func (l *SIPLeg) sendAck() {
	msg := l.baseRequest("ACK", 1, "", "")
	_, _ = l.sipConn.WriteToUDP([]byte(msg), l.remoteSIP)
}

func (l *SIPLeg) sendBye() {
	msg := l.baseRequest("BYE", 2, "", "")
	_, _ = l.sipConn.WriteToUDP([]byte(msg), l.remoteSIP)
}

func (l *SIPLeg) sendCancel() {
	msg := l.baseRequest("CANCEL", 1, "", "")
	_, _ = l.sipConn.WriteToUDP([]byte(msg), l.remoteSIP)
}

func (l *SIPLeg) baseRequest(method string, cseq int, body, contentType string) string {
	var b strings.Builder
	b.WriteString(method + " " + l.targetURI + " SIP/2.0\r\n")
	b.WriteString("Via: SIP/2.0/UDP " + l.localIP + ":" + strconv.Itoa(l.sipPort) + ";branch=" + l.branch + "\r\n")
	b.WriteString("Max-Forwards: 70\r\n")
	b.WriteString("From: <" + l.fromURI + ">;tag=" + l.fromTag + "\r\n")
	b.WriteString("To: <" + l.targetURI + ">")
	if l.toTag != "" {
		b.WriteString(";tag=" + l.toTag)
	}
	b.WriteString("\r\n")
	b.WriteString("Call-ID: " + l.sipID + "\r\n")
	b.WriteString("CSeq: " + strconv.Itoa(cseq) + " " + method + "\r\n")
	b.WriteString("Contact: " + l.contact + "\r\n")
	b.WriteString("User-Agent: WaCalls-Asterisk-Gateway\r\n")
	if contentType != "" {
		b.WriteString("Content-Type: " + contentType + "\r\n")
	}
	b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
	b.WriteString(body)
	return b.String()
}

func (l *SIPLeg) localSDP() string {
	return "v=0\r\n" +
		"o=wacalls 0 0 IN IP4 " + l.localIP + "\r\n" +
		"s=WaCalls\r\n" +
		"c=IN IP4 " + l.localIP + "\r\n" +
		"t=0 0\r\n" +
		"m=audio " + strconv.Itoa(l.rtpPort) + " RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=sendrecv\r\n"
}

func (l *SIPLeg) sendSIPResponse(req string, code int, text string) {
	headers := sipHeaders(req)
	var b strings.Builder
	b.WriteString("SIP/2.0 " + strconv.Itoa(code) + " " + text + "\r\n")
	for _, name := range []string{"via", "from", "to", "call-id", "cseq"} {
		if v := headers[name]; v != "" {
			b.WriteString(canonicalHeader(name) + ": " + v + "\r\n")
		}
	}
	b.WriteString("Content-Length: 0\r\n\r\n")
	_, _ = l.sipConn.WriteToUDP([]byte(b.String()), l.remoteSIP)
}

func parseSIPTarget(raw string) (string, *net.UDPAddr, error) {
	target := strings.TrimSpace(strings.TrimPrefix(raw, "sip:"))
	if target == "" {
		return "", nil, fmt.Errorf("asterisk SIP target is empty")
	}
	hostport := target
	if at := strings.LastIndex(target, "@"); at >= 0 {
		hostport = target[at+1:]
	}
	if !strings.Contains(hostport, ":") {
		hostport += ":5060"
	}
	addr, err := net.ResolveUDPAddr("udp", hostport)
	if err != nil {
		return "", nil, fmt.Errorf("resolve SIP target: %w", err)
	}
	return "sip:" + target, addr, nil
}

func mustUDPAddr(raw string) *net.UDPAddr {
	addr, err := net.ResolveUDPAddr("udp", raw)
	if err != nil {
		return &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	}
	return addr
}

func outboundIP(remote *net.UDPAddr) string {
	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func sipStatusCode(msg string) int {
	line, _, _ := strings.Cut(msg, "\r\n")
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	code, _ := strconv.Atoi(fields[1])
	return code
}

func sipHeaders(msg string) map[string]string {
	headers := map[string]string{}
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	return headers
}

func canonicalHeader(name string) string {
	switch name {
	case "call-id":
		return "Call-ID"
	case "cseq":
		return "CSeq"
	default:
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

func headerParam(header, key string) string {
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(k, key) {
			return strings.Trim(v, "\"")
		}
	}
	return ""
}

func parseSDPRTP(msg, fallbackIP string) (*net.UDPAddr, error) {
	_, body, ok := strings.Cut(msg, "\r\n\r\n")
	if !ok {
		return nil, fmt.Errorf("missing SDP")
	}
	ip := fallbackIP
	port := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "c=IN IP4 ") {
			ip = strings.TrimSpace(strings.TrimPrefix(line, "c=IN IP4 "))
		}
		if strings.HasPrefix(line, "m=audio ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				port, _ = strconv.Atoi(fields[1])
			}
		}
	}
	if port == 0 {
		return nil, fmt.Errorf("missing audio port")
	}
	return net.ResolveUDPAddr("udp", net.JoinHostPort(ip, strconv.Itoa(port)))
}

func token(n int) string {
	const alphabet = "0123456789abcdef"
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf)
}

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

func downsample16To8(in []float32) []float32 {
	out := make([]float32, len(in)/2)
	for i := range out {
		out[i] = (in[i*2] + in[i*2+1]) * 0.5
	}
	return out
}

func upsample8To16(in []float32) []float32 {
	out := make([]float32, len(in)*2)
	for i, v := range in {
		out[i*2] = v
		out[i*2+1] = v
	}
	return out
}

func floatToInt16(s float32) int16 {
	switch {
	case s > 1:
		return 32767
	case s < -1:
		return -32768
	}
	return int16(s * 32767)
}

func linearToULaw(sample int16) byte {
	const bias = 0x84
	const clip = 32635
	sign := 0
	pcm := int(sample)
	if pcm < 0 {
		pcm = -pcm
		sign = 0x80
	}
	if pcm > clip {
		pcm = clip
	}
	pcm += bias
	exponent := 7
	for mask := 0x4000; exponent > 0 && pcm&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := (pcm >> uint(exponent+3)) & 0x0f
	return ^byte(sign | exponent<<4 | mantissa)
}

func uLawToLinear(u byte) int16 {
	const bias = 0x84
	u = ^u
	t := ((int(u&0x0f) << 3) + bias) << uint((u&0x70)>>4)
	if u&0x80 != 0 {
		return int16(bias - t)
	}
	return int16(t - bias)
}
