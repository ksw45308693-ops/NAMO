package web

import (
	"errors"
	"net/http"

	"namo/internal/config"
)

func (h *Handler) handleSaveAPIKey(w http.ResponseWriter, r *http.Request, identity RequestContext) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if h.demo {
		demoNotImplemented(w)
		return
	}
	if !canViewAdmin(identity) || identity.UserID == "" {
		http.Error(w, "플랫폼 관리자 권한이 필요합니다.", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		status := http.StatusBadRequest
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "입력 내용을 확인해 주세요.", status)
		return
	}
	// Authentication middleware may already have parsed the request body.
	if len(r.PostForm.Encode()) > 16*1024 {
		http.Error(w, "입력 내용이 너무 깁니다.", http.StatusRequestEntityTooLarge)
		return
	}
	if !validCSRF(r, identity) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	key, err := config.NormalizeAPIKey(r.PostForm.Get("api_key"))
	if err != nil {
		http.Error(w, config.ErrInvalidAPIKey.Error(), http.StatusBadRequest)
		return
	}
	if h.saveAPIKey == nil {
		http.Error(w, "API 키 저장 기능을 사용할 수 없습니다.", http.StatusServiceUnavailable)
		return
	}
	if err := h.saveAPIKey(r.Context(), identity, key); err != nil {
		http.Error(w, "API 키를 저장하지 못했습니다. 서버의 저장 경로와 권한을 확인해 주세요.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?result=g2b-key-saved", http.StatusSeeOther)
}
