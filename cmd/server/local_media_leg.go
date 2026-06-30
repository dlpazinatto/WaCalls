package main

type LocalMediaLeg interface {
	WritePCM([]float32) error
	Close()
}

type AnswerableLocalMediaLeg interface {
	Answer()
}

func (s *Session) answerLocalMediaLeg(callID string) {
	ac, ok := s.reg.get(callID)
	if !ok || ac.leg == nil {
		return
	}
	answerer, ok := ac.leg.(AnswerableLocalMediaLeg)
	if ok {
		answerer.Answer()
	}
}

func (s *Session) writeLocalMediaLegPCM(callID string, pcm []float32) {
	ac, ok := s.reg.get(callID)
	if !ok || ac.leg == nil {
		return
	}
	_ = ac.leg.WritePCM(pcm)
}

func closeLocalMediaLeg(ac *activeCall) {
	if ac != nil && ac.leg != nil {
		ac.leg.Close()
	}
}
