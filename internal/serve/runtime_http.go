package serve

import (
	"net/http"
)

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
