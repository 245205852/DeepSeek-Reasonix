package main

import (
	"context"
	"encoding/json"
	"net/http"

	"reasonix/internal/control"
)

const attachmentUploadPath = "/__reasonix_attachment_upload"

func (a *App) attachmentUploadMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != attachmentUploadPath {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("Content-Type") != "application/octet-stream" {
				http.Error(w, "unsupported upload type", http.StatusUnsupportedMediaType)
				return
			}
			target, err := a.attachmentTargetToken(r.URL.Query().Get("target"))
			if err != nil {
				http.Error(w, "attachment target is unavailable", http.StatusConflict)
				return
			}
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			stop := context.AfterFunc(a.attachmentOperationContext(target), func() { cancel(); _ = r.Body.Close() })
			defer stop()
			rel, err := control.SaveAttachmentStreamInRoot(ctx, target.root, r.URL.Query().Get("name"), r.Body)
			rel, err = a.finishAttachmentWrite(target, rel, err)
			if err != nil {
				status := http.StatusInternalServerError
				if !a.attachmentTargetCurrent(target) {
					status = http.StatusConflict
				}
				http.Error(w, "attachment upload could not be saved", status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				Path string `json:"path"`
			}{Path: rel})
		})
	}
}
