package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sky22333/qqbot/config"
	"github.com/sky22333/qqbot/internal/notifier"
	"github.com/sky22333/qqbot/internal/targets"
	"github.com/sky22333/qqbot/message"
)

type Server struct {
	cfg      config.Config
	logger   *slog.Logger
	notifier *notifier.Notifier
	targets  *targets.Store
	srv      *http.Server
}

func New(cfg config.Config, logger *slog.Logger, n *notifier.Notifier, t *targets.Store) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	readTimeout, err := cfg.ReadTimeout()
	if err != nil {
		return nil, err
	}
	writeTimeout, err := cfg.WriteTimeout()
	if err != nil {
		return nil, err
	}
	handler := &Server{
		cfg:      cfg,
		logger:   logger,
		notifier: n,
		targets:  t,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handler.healthz)
	mux.HandleFunc("/readyz", handler.readyz)
	mux.HandleFunc("/api/v1/messages/send", handler.sendSync)
	mux.HandleFunc("/api/v1/messages", handler.enqueue)
	mux.HandleFunc("/api/v1/messages/", handler.queryStatus)
	mux.HandleFunc("/api/v1/targets", handler.listTargets)

	handler.srv = &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      mux,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	}
	return handler, nil
}

func (s *Server) Start() error {
	s.logger.Info("HTTP 服务启动", "addr", s.cfg.Server.ListenAddr)
	err := s.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now()})
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "time": time.Now()})
}

func (s *Server) sendSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	req, err := s.decodePushRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.notifier.Send(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":    0,
		"message": "ok",
		"data":    result,
	})
}

func (s *Server) enqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	req, err := s.decodePushRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	requestID, err := s.notifier.Enqueue(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"code":    0,
		"message": "accepted",
		"data": map[string]any{
			"request_id": requestID,
		},
	})
}

func (s *Server) queryStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	requestID := strings.TrimPrefix(r.URL.Path, "/api/v1/messages/")
	if requestID == "" {
		writeError(w, http.StatusBadRequest, "request id 不能为空")
		return
	}
	status, ok := s.notifier.GetStatus(requestID)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":    0,
		"message": "ok",
		"data":    status,
	})
}

func (s *Server) listTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	targetType := strings.TrimSpace(r.URL.Query().Get("target_type"))
	items := s.targets.List(200, targetType)
	writeJSON(w, http.StatusOK, map[string]any{
		"code":    0,
		"message": "ok",
		"data":    items,
	})
}

func (s *Server) decodePushRequest(w http.ResponseWriter, r *http.Request) (message.PushRequest, error) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	req := message.PushRequest{}
	if err := decoder.Decode(&req); err != nil {
		return message.PushRequest{}, err
	}
	return req, nil
}

func (s *Server) authorized(r *http.Request) bool {
	if strings.TrimSpace(s.cfg.Server.APIToken) == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	return token == s.cfg.Server.APIToken
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"code":    status,
		"message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
