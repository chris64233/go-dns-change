// Package httpapi 把 dnschange.Service 暴露为 REST 接口。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	dns "github.com/chris64233/go-dns-change"
)

// Handler 持有服务依赖并实现 http.Handler。
type Handler struct {
	svc *dns.Service
	mux *http.ServeMux
}

// NewHandler 创建路由就绪的 Handler。
func NewHandler(svc *dns.Service) *Handler {
	h := &Handler{svc: svc, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /zones", h.initZone)
	h.mux.HandleFunc("GET /zones/{zone}", h.getZone)
	h.mux.HandleFunc("PUT /zones/{zone}/policy", h.updatePolicy)
	h.mux.HandleFunc("POST /zones/{zone}/changes", h.submitChange)
	h.mux.HandleFunc("POST /zones/{zone}/revisions/{revision}/approve", h.approve)
	h.mux.HandleFunc("POST /zones/{zone}/revisions/{revision}/publish", h.publish)
	h.mux.HandleFunc("POST /zones/{zone}/revisions/{revision}/cancel", h.cancel)
	h.mux.HandleFunc("POST /zones/{zone}/rollbacks", h.rollback)
	h.mux.HandleFunc("GET /zones/{zone}/revisions", h.listRevisions)
	h.mux.HandleFunc("GET /zones/{zone}/revisions/{revision}", h.getRevision)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) initZone(w http.ResponseWriter, r *http.Request) {
	var req dns.InitZoneRequest
	if !decode(w, r, &req) {
		return
	}
	rev, event, err := h.svc.InitZone(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"revision": rev, "outbox_event": event})
}

func (h *Handler) getZone(w http.ResponseWriter, r *http.Request) {
	view, err := h.svc.GetZone(r.Context(), r.PathValue("zone"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) updatePolicy(w http.ResponseWriter, r *http.Request) {
	var p dns.Policy
	if !decode(w, r, &p) {
		return
	}
	out, err := h.svc.UpdatePolicy(r.Context(), r.PathValue("zone"), p)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) submitChange(w http.ResponseWriter, r *http.Request) {
	var req dns.SubmitChangeRequest
	if !decode(w, r, &req) {
		return
	}
	req.Zone = r.PathValue("zone")
	res, err := h.svc.SubmitChange(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"revision": res.Revision, "replayed": res.Replayed})
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	rev, ok := revisionFromPath(w, r)
	if !ok {
		return
	}
	var req dns.ApproveRequest
	if !decode(w, r, &req) {
		return
	}
	req.Zone = r.PathValue("zone")
	req.Revision = rev
	res, err := h.svc.Approve(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"revision": res.Revision, "approval": res.Approval, "replayed": res.Replayed})
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	rev, ok := revisionFromPath(w, r)
	if !ok {
		return
	}
	res, err := h.svc.Publish(r.Context(), dns.PublishRequest{Zone: r.PathValue("zone"), Revision: rev})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": res.Revision, "outbox_event": res.OutboxEvent})
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	rev, ok := revisionFromPath(w, r)
	if !ok {
		return
	}
	var req dns.CancelRequest
	if r.ContentLength != 0 {
		if !decode(w, r, &req) {
			return
		}
	}
	req.Zone = r.PathValue("zone")
	req.Revision = rev
	out, err := h.svc.Cancel(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) rollback(w http.ResponseWriter, r *http.Request) {
	var req dns.RollbackRequest
	if !decode(w, r, &req) {
		return
	}
	req.Zone = r.PathValue("zone")
	res, err := h.svc.Rollback(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"revision": res.Revision, "replayed": res.Replayed})
}

func (h *Handler) getRevision(w http.ResponseWriter, r *http.Request) {
	rev, ok := revisionFromPath(w, r)
	if !ok {
		return
	}
	out, err := h.svc.GetRevision(r.Context(), r.PathValue("zone"), rev)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) listRevisions(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListRevisions(r.Context(), r.PathValue("zone"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad_request", "message": err.Error()})
		return false
	}
	return true
}

func revisionFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || n <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad_request", "message": "revision must be a positive integer"})
		return 0, false
	}
	return n, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorStatus 把领域错误码映射到 HTTP 状态。
func errorStatus(err error) int {
	switch dns.CodeOf(err) {
	case dns.ErrValidation:
		return http.StatusUnprocessableEntity
	case dns.ErrRevision:
		// 409 用于基准冲突等；“不存在”由消息内容区分，这里统一 409 以保留错误语义分组。
		if strings.Contains(err.Error(), "does not exist") {
			return http.StatusNotFound
		}
		return http.StatusConflict
	case dns.ErrApproval:
		return http.StatusForbidden
	case dns.ErrState:
		return http.StatusConflict
	case dns.ErrIdempotency:
		return http.StatusConflict
	default:
		if errors.Is(err, strconv.ErrSyntax) {
			return http.StatusBadRequest
		}
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, err error) {
	code := dns.CodeOf(err)
	status := errorStatus(err)
	if code == "" {
		code = "internal_error"
	}
	writeJSON(w, status, map[string]string{"code": string(code), "message": err.Error()})
}
