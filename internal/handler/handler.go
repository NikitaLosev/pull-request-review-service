package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/trainee/review-service/internal/model"
	"github.com/trainee/review-service/internal/service"
)

type Handler struct {
	service *service.Service
}

func NewHandler(s *service.Service) *Handler {
	return &Handler{service: s}
}

func (h *Handler) SetupRouter() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.SetHeader("Content-Type", "application/json"))
	r.Use(prometheusMiddleware)

	// Health
	r.Get("/health", h.Health)
	// Metrics (Prometheus)
	r.Handle("/metrics", promhttp.Handler())

	// Teams
	r.Post("/team/add", h.CreateTeam)
	r.Get("/team/get", h.GetTeam)

	// Users
	r.Post("/users/setIsActive", h.SetUserActivity)
	r.Get("/users/getReview", h.GetUserReviews)

	// PullRequests
	r.Post("/pullRequest/create", h.CreatePR)
	r.Post("/pullRequest/merge", h.MergePR)
	r.Post("/pullRequest/reassign", h.ReassignReviewer)

	return r
}

// --- Хелперы для ответов ---

func respondJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload != nil {
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			slog.Error("Failed to encode response", "error", err)
		}
	}
}

type APIErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// respondError маппит доменные ошибки на HTTP статусы и коды ошибок OpenAPI.
func respondError(w http.ResponseWriter, err error) {
	var code string
	message := err.Error()
	status := http.StatusInternalServerError

	switch {
	// 404 Not Found
	case errors.Is(err, model.ErrNotFound):
		status = http.StatusNotFound
		code = "NOT_FOUND"

	// 400 Bad Request
	case errors.Is(err, model.ErrBadRequest):
		status = http.StatusBadRequest
		code = "BAD_REQUEST"
	case errors.Is(err, model.ErrTeamExists):
		status = http.StatusConflict
		code = "TEAM_EXISTS"

	// 409 Conflict
	case errors.Is(err, model.ErrPRExists):
		status = http.StatusConflict
		code = "PR_EXISTS"
	case errors.Is(err, model.ErrPRMerged):
		status = http.StatusConflict
		code = "PR_MERGED"
	case errors.Is(err, model.ErrNotAssigned):
		status = http.StatusConflict
		code = "NOT_ASSIGNED"
	case errors.Is(err, model.ErrNoCandidate):
		status = http.StatusConflict
		code = "NO_CANDIDATE"

	// 500 Internal Server Error
	default:
		slog.Error("Internal Server Error", "error", err)
		code = "INTERNAL_ERROR"
		message = "An unexpected error occurred"
	}

	payload := APIErrorResponse{}
	payload.Error.Code = code
	payload.Error.Message = message
	respondJSON(w, status, payload)
}

func decode(r *http.Request, v interface{}) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return model.ErrBadRequest
	}
	return nil
}

// GET /health
func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Реализация обработчиков ---

// POST /team/add
func (h *Handler) CreateTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TeamName string `json:"team_name"`
		Members  []struct {
			UserID   string `json:"user_id"`
			Username string `json:"username"`
			IsActive *bool  `json:"is_active"`
		} `json:"members"`
	}
	if err := decode(r, &req); err != nil {
		respondError(w, err)
		return
	}

	if strings.TrimSpace(req.TeamName) == "" {
		respondError(w, model.ErrBadRequest)
		return
	}

	seen := make(map[string]struct{}, len(req.Members))
	team := model.Team{
		TeamName: req.TeamName,
		Members:  make([]model.TeamMember, 0, len(req.Members)),
	}

	for _, m := range req.Members {
		if strings.TrimSpace(m.UserID) == "" || strings.TrimSpace(m.Username) == "" || m.IsActive == nil {
			respondError(w, model.ErrBadRequest)
			return
		}
		if _, ok := seen[m.UserID]; ok {
			respondError(w, model.ErrBadRequest)
			return
		}
		seen[m.UserID] = struct{}{}
		team.Members = append(team.Members, model.TeamMember{
			UserID:   m.UserID,
			Username: m.Username,
			IsActive: *m.IsActive,
		})
	}

	if err := h.service.CreateTeam(r.Context(), team); err != nil {
		respondError(w, err)
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{"team": team})
}

// GET /team/get
func (h *Handler) GetTeam(w http.ResponseWriter, r *http.Request) {
	teamName := r.URL.Query().Get("team_name")
	if teamName == "" {
		respondError(w, model.ErrBadRequest)
		return
	}

	team, err := h.service.GetTeam(r.Context(), teamName)
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, team)
}

// POST /users/setIsActive
func (h *Handler) SetUserActivity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID   string `json:"user_id"`
		IsActive *bool  `json:"is_active"`
	}
	if err := decode(r, &req); err != nil {
		respondError(w, err)
		return
	}

	if strings.TrimSpace(req.UserID) == "" || req.IsActive == nil {
		respondError(w, model.ErrBadRequest)
		return
	}

	user, err := h.service.SetUserActiveStatus(r.Context(), req.UserID, *req.IsActive)
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"user": user})
}

// GET /users/getReview
func (h *Handler) GetUserReviews(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		respondError(w, model.ErrBadRequest)
		return
	}

	prs, err := h.service.GetUserReviewPRs(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return
	}

	// Гарантируем, что возвращается пустой массив, а не null
	if prs == nil {
		prs = []model.PullRequestShort{}
	}

	response := map[string]interface{}{
		"user_id":       userID,
		"pull_requests": prs,
	}
	respondJSON(w, http.StatusOK, response)
}

// POST /pullRequest/create
func (h *Handler) CreatePR(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"pull_request_id"`
		Name     string `json:"pull_request_name"`
		AuthorID string `json:"author_id"`
	}
	if err := decode(r, &req); err != nil {
		respondError(w, err)
		return
	}

	if strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.AuthorID) == "" || strings.TrimSpace(req.Name) == "" {
		respondError(w, model.ErrBadRequest)
		return
	}

	pr, err := h.service.CreatePullRequest(r.Context(), req.ID, req.Name, req.AuthorID)
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]interface{}{"pr": pr})
}

// POST /pullRequest/merge
func (h *Handler) MergePR(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"pull_request_id"`
	}
	if err := decode(r, &req); err != nil {
		respondError(w, err)
		return
	}

	if strings.TrimSpace(req.ID) == "" {
		respondError(w, model.ErrBadRequest)
		return
	}

	pr, err := h.service.MergePullRequest(r.Context(), req.ID)
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"pr": pr})
}

// POST /pullRequest/reassign
func (h *Handler) ReassignReviewer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PRID string `json:"pull_request_id"`
		// Следуем схеме OpenAPI (old_user_id), но также поддерживаем пример old_reviewer_id для совместимости.
		OldUserID string `json:"old_user_id"`
		LegacyOld string `json:"old_reviewer_id"`
	}

	if err := decode(r, &req); err != nil {
		respondError(w, err)
		return
	}

	if req.OldUserID == "" {
		req.OldUserID = req.LegacyOld
	}

	if strings.TrimSpace(req.PRID) == "" || strings.TrimSpace(req.OldUserID) == "" {
		respondError(w, model.ErrBadRequest)
		return
	}

	pr, replacedBy, err := h.service.ReassignReviewer(r.Context(), req.PRID, req.OldUserID)
	if err != nil {
		respondError(w, err)
		return
	}

	response := map[string]interface{}{
		"pr":          pr,
		"replaced_by": replacedBy,
	}
	respondJSON(w, http.StatusOK, response)
}
