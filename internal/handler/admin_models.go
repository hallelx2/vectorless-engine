package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hallelx2/vectorless-engine/internal/middleware"
	"github.com/hallelx2/vectorless-engine/pkg/models"
)

// codeModelsNotConfigured is the error code a caller can branch on.
const codeModelsNotConfigured = "models_not_configured"

// writeModelsNotConfigured answers 503 with a typed error body.
func writeModelsNotConfigured(w http.ResponseWriter, err error) {
	msg := (&models.NotConfiguredError{Kind: "generative"}).Error()
	if err != nil {
		msg = err.Error()
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": msg,
		"code":  codeModelsNotConfigured,
	})
}

// RequireModels answers 503 models_not_configured while the registry has
// no generative model. A nil registry passes every request through.
func RequireModels(reg *models.Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if reg == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodOptions && !reg.GenerativeConfigured() {
				writeModelsNotConfigured(w, nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireModelsForIngest gates ingest the same way, except in minimal
// mode, which reads no model. Over Connect it gates only the ingest
// method, so listing and reading documents keep working.
func RequireModelsForIngest(reg *models.Registry, mode string) func(http.Handler) http.Handler {
	if mode == "minimal" {
		return func(next http.Handler) http.Handler { return next }
	}
	gate := RequireModels(reg)
	return func(next http.Handler) http.Handler {
		gated := gate(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "DocumentsService/") && !strings.Contains(r.URL.Path, "Ingest") {
				next.ServeHTTP(w, r)
				return
			}
			gated.ServeHTTP(w, r)
		})
	}
}

// RequireEngineToken admits only a caller authenticated with the
// engine's API key. An engine running with auth.mode=none has no admin
// API: anyone who could reach it could replace its provider keys.
func RequireEngineToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := middleware.PrincipalFromContext(r.Context())
		if !p.Authenticated || p.ID != "api_key" {
			writeErr(w, http.StatusForbidden, "admin API requires the engine token (auth.mode=api_key)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ModelsStatusHandler serves the redacted status to any authenticated
// caller. A nil registry reports nothing configured.
func ModelsStatusHandler(reg *models.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reg == nil {
			writeJSON(w, http.StatusOK, models.Status{})
			return
		}
		writeJSON(w, http.StatusOK, reg.Status())
	}
}

// AdminModelsHandler serves /admin/v1/models.
type AdminModelsHandler struct {
	logger *slog.Logger
	reg    *models.Registry
}

// NewAdminModelsHandler builds the handler.
func NewAdminModelsHandler(logger *slog.Logger, reg *models.Registry) *AdminModelsHandler {
	return &AdminModelsHandler{logger: logger, reg: reg}
}

// adminActor names who made a change, as the control plane reports it.
func adminActor(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Vectorless-Actor")); v != "" {
		if len(v) > 200 {
			v = v[:200]
		}
		return v
	}
	return "engine-token"
}

// HandleGet returns the redacted status.
func (h *AdminModelsHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.reg.Status())
}

// HandlePut applies an update and returns the new status. The new
// providers are in effect when it returns.
func (h *AdminModelsHandler) HandlePut(w http.ResponseWriter, r *http.Request) {
	var u models.Update
	if err := decodeAdminBody(r, &u); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	st, err := h.reg.Save(r.Context(), u, adminActor(r))
	if err != nil {
		var ve *models.ValidationError
		switch {
		case errors.As(err, &ve):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": ve.Error(), "code": "invalid_settings"})
		case errors.Is(err, models.ErrNoEncryptionKey):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "no_encryption_key"})
		default:
			h.logger.Error("admin: save model settings", "err", err)
			writeErr(w, http.StatusInternalServerError, "could not save model settings")
		}
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// HandleTest makes a tiny real call to each provider: the ones in effect
// for an empty body, or the ones an update would put in effect.
func (h *AdminModelsHandler) HandleTest(w http.ResponseWriter, r *http.Request) {
	var u *models.Update
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		u = &models.Update{}
		if err := json.Unmarshal(body, u); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	res, err := h.reg.Test(r.Context(), u)
	if err != nil {
		var ve *models.ValidationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": ve.Error(), "code": "invalid_settings"})
			return
		}
		writeErr(w, http.StatusInternalServerError, "connection test failed to run")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func decodeAdminBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}
