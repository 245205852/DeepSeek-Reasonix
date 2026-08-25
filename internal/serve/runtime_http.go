package serve

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) modelSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ref string `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Ref) == "" {
		http.Error(w, "missing model ref", http.StatusBadRequest)
		return
	}
	if err := s.switchModel(r.Context(), strings.TrimSpace(body.Ref)); err != nil {
		http.Error(w, err.Error(), runtimeSwitchErrorStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) effortSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Level) == "" {
		http.Error(w, "missing effort level", http.StatusBadRequest)
		return
	}
	if err := s.switchEffort(r.Context(), strings.TrimSpace(body.Level)); err != nil {
		http.Error(w, err.Error(), runtimeSwitchErrorStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) goalPause(w http.ResponseWriter, _ *http.Request) {
	if !s.ctl().PauseGoal() {
		http.Error(w, "the active goal cannot be paused", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) goalResume(w http.ResponseWriter, _ *http.Request) {
	if !s.ctl().ResumeGoal() {
		http.Error(w, "the active goal cannot be resumed", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) jobsCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid job cancellation request", http.StatusBadRequest)
		return
	}
	canceller, ok := any(s.ctl()).(interface{ CancelJob(string) bool })
	if !ok {
		http.Error(w, "background job cancellation is unavailable", http.StatusConflict)
		return
	}
	cancelled := []string{}
	notRunning := []string{}
	seen := map[string]bool{}
	for _, raw := range body.IDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if canceller.CancelJob(id) {
			cancelled = append(cancelled, id)
		} else {
			notRunning = append(notRunning, id)
		}
	}
	if len(seen) == 0 {
		http.Error(w, "at least one job id is required", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"cancelled": cancelled, "notRunning": notRunning})
}

func runtimeSwitchErrorStatus(err error) int {
	message := err.Error()
	if strings.Contains(message, "active work") || strings.Contains(message, "session in use") || strings.Contains(message, "session changed") {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// providersReload rebuilds the current model after an on-disk provider or
// credential-tunnel endpoint change. Busy serves return a retryable 409.
func (s *Server) providersReload(w http.ResponseWriter, r *http.Request) {
	ref := s.ctl().ModelRef()
	if err := s.switchModel(r.Context(), ref); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]string{"model": ref})
}
