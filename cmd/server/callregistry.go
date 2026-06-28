package main

import (
	"sync"

	"wacalls/internal/voip/call"
)

type activeCall struct {
	cm  *call.CallManager
	leg LocalMediaLeg
}

type LocalMediaLeg interface {
	WritePCM([]float32) error
	Close()
}

type callRegistry struct {
	mu    sync.Mutex
	calls map[string]*activeCall
}

func newCallRegistry() *callRegistry {
	return &callRegistry{calls: map[string]*activeCall{}}
}

func (r *callRegistry) add(callID string, ac *activeCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[callID] = ac
}

func (r *callRegistry) get(callID string) (*activeCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac, ok := r.calls[callID]
	return ac, ok
}

func (r *callRegistry) remove(callID string) (*activeCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac, ok := r.calls[callID]
	if !ok {
		return nil, false
	}
	delete(r.calls, callID)
	return ac, true
}

func (r *callRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *callRegistry) setLeg(callID string, leg LocalMediaLeg) (LocalMediaLeg, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac, ok := r.calls[callID]
	if !ok {
		return nil, false
	}
	old := ac.leg
	ac.leg = leg
	return old, true
}

func (r *callRegistry) setBridge(callID string, b *Bridge) (*Bridge, bool) {
	old, ok := r.setLeg(callID, b)
	if old == nil {
		return nil, ok
	}
	bridge, _ := old.(*Bridge)
	return bridge, ok
}

func (r *callRegistry) drain() []*activeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*activeCall, 0, len(r.calls))
	for _, ac := range r.calls {
		out = append(out, ac)
	}
	r.calls = map[string]*activeCall{}
	return out
}
