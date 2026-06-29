package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

type AsteriskDefaults struct {
	DefaultTarget string
	DefaultFrom   string
	ListenBind    string
	SIPBind       string
	RTPBind       string
	AdvertiseIP   string
	RTPMin        int
	RTPMax        int
}

type AsteriskGateway struct {
	defaults AsteriskDefaults
	store    *asteriskRouteStore
	rtpPool  *rtpPortPool
}

type AsteriskCallRoute struct {
	SIPConfig SIPConfig
	ToUser    string
	FromUser  string
	Release   func()
}

func NewAsteriskGateway(defaults AsteriskDefaults, store *asteriskRouteStore) *AsteriskGateway {
	return &AsteriskGateway{
		defaults: defaults,
		store:    store,
		rtpPool:  newRTPPortPool(defaults.RTPMin, defaults.RTPMax),
	}
}

func (g *AsteriskGateway) configForCall(ctx context.Context, sessionID, waNumber string) (AsteriskCallRoute, bool, error) {
	route, ok, err := g.store.findForSession(ctx, sessionID, waNumber)
	if err != nil {
		return AsteriskCallRoute{}, false, err
	}
	target := g.defaults.DefaultTarget
	from := g.defaults.DefaultFrom
	toUser := ""
	fromUser := ""
	if ok {
		target = route.SIPTarget
		from = route.SIPFrom
		toUser = route.ToUser
		fromUser = route.FromUser
	}
	if strings.TrimSpace(target) == "" {
		return AsteriskCallRoute{}, false, nil
	}
	port, release, err := g.rtpPool.acquire()
	if err != nil {
		return AsteriskCallRoute{}, false, err
	}
	rtpBind := g.defaults.RTPBind
	if port != 0 {
		rtpBind = ":" + strconv.Itoa(port)
	}
	cfg := SIPConfig{
		Target:      target,
		FromUser:    from,
		Bind:        g.defaults.SIPBind,
		RTPBind:     rtpBind,
		AdvertiseIP: g.defaults.AdvertiseIP,
	}
	return AsteriskCallRoute{SIPConfig: cfg, ToUser: toUser, FromUser: fromUser, Release: release}, true, nil
}

func (g *AsteriskGateway) acquireRTPBind() (string, func(), error) {
	port, release, err := g.rtpPool.acquire()
	if err != nil {
		return "", nil, err
	}
	if port != 0 {
		return ":" + strconv.Itoa(port), release, nil
	}
	return g.defaults.RTPBind, release, nil
}

type rtpPortPool struct {
	mu    sync.Mutex
	min   int
	max   int
	next  int
	inUse map[int]bool
}

func newRTPPortPool(minPort, maxPort int) *rtpPortPool {
	if minPort <= 0 || maxPort < minPort {
		minPort, maxPort = 0, 0
	}
	return &rtpPortPool{min: minPort, max: maxPort, next: minPort, inUse: map[int]bool{}}
}

func (p *rtpPortPool) acquire() (int, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.min == 0 {
		return 0, func() {}, nil
	}
	size := p.max - p.min + 1
	for i := 0; i < size; i++ {
		port := p.next
		p.next++
		if p.next > p.max {
			p.next = p.min
		}
		if p.inUse[port] {
			continue
		}
		p.inUse[port] = true
		return port, func() { p.release(port) }, nil
	}
	return 0, nil, fmt.Errorf("no free RTP ports in range %d-%d", p.min, p.max)
}

func (p *rtpPortPool) release(port int) {
	if port == 0 {
		return
	}
	p.mu.Lock()
	delete(p.inUse, port)
	p.mu.Unlock()
}
