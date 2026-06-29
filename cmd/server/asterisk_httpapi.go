package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (s *server) handleAsteriskPairedSessions(w http.ResponseWriter, r *http.Request) {
	type row struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
		JID       string `json:"jid"`
		Number    string `json:"number"`
		State     string `json:"state"`
		Paired    bool   `json:"paired"`
	}
	infos := s.sessions.infos()
	out := make([]row, 0, len(infos))
	for _, info := range infos {
		if !info.Paired || info.JID == "" {
			continue
		}
		out = append(out, row{
			SessionID: info.ID,
			Name:      info.Name,
			JID:       info.JID,
			Number:    sipUserFromJIDString(info.JID),
			State:     info.State,
			Paired:    info.Paired,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *server) handleAsteriskRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routes, err := s.asterisk.store.list(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"routes": routes})
	case http.MethodPost:
		route, ok := decodeAsteriskRoute(w, r, true)
		if !ok {
			return
		}
		created, err := s.asterisk.store.insert(r.Context(), route)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"route": created})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *server) handleAsteriskRouteByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodPut:
		route, ok := decodeAsteriskRoute(w, r, false)
		if !ok {
			return
		}
		updated, found, err := s.asterisk.store.update(r.Context(), id, route)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such route"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"route": updated})
	case http.MethodDelete:
		found, err := s.asterisk.store.delete(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such route"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func decodeAsteriskRoute(w http.ResponseWriter, r *http.Request, defaultEnabled bool) (AsteriskRoute, bool) {
	var body struct {
		SessionID string `json:"sessionId"`
		WANumber  string `json:"waNumber"`
		SIPServer string `json:"sipServer"`
		SIPTarget string `json:"sipTarget"`
		SIPFrom   string `json:"sipFrom"`
		Enabled   *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return AsteriskRoute{}, false
	}
	route := AsteriskRoute{
		SessionID: strings.TrimSpace(body.SessionID),
		WANumber:  normalizePhone(body.WANumber),
		SIPTarget: strings.TrimSpace(body.SIPServer),
		SIPFrom:   strings.TrimSpace(body.SIPFrom),
		Enabled:   defaultEnabled,
	}
	if route.SIPTarget == "" {
		route.SIPTarget = strings.TrimSpace(body.SIPTarget)
	}
	if body.Enabled != nil {
		route.Enabled = *body.Enabled
	}
	if route.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sessionId required"})
		return AsteriskRoute{}, false
	}
	if route.WANumber == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "waNumber required"})
		return AsteriskRoute{}, false
	}
	if route.SIPTarget == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sipServer required"})
		return AsteriskRoute{}, false
	}
	return route, true
}
