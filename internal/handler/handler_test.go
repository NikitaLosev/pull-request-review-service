package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trainee/review-service/internal/model"
	repo "github.com/trainee/review-service/internal/repository"
	"github.com/trainee/review-service/internal/service"
)

// --- Тестовый in-memory репозиторий, реализующий интерфейс service.Repository.

type fakeRepo struct {
	mu    sync.Mutex
	teams map[string]model.Team
	users map[string]model.User
	prs   map[string]*model.PullRequest
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		teams: make(map[string]model.Team),
		users: make(map[string]model.User),
		prs:   make(map[string]*model.PullRequest),
	}
}

func (f *fakeRepo) WithTransaction(_ context.Context, fn func(repo.TxRepository) error) error {
	return fn(f)
}

func (f *fakeRepo) CreateTeamTx(_ context.Context, team model.Team) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.teams[team.TeamName]; exists {
		return repo.ErrAlreadyExists
	}
	f.teams[team.TeamName] = team
	for _, m := range team.Members {
		f.users[m.UserID] = model.User{
			UserID:   m.UserID,
			Username: m.Username,
			TeamName: team.TeamName,
			IsActive: m.IsActive,
		}
	}
	return nil
}

func (f *fakeRepo) GetTeam(_ context.Context, teamName string) (*model.Team, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	team, ok := f.teams[teamName]
	if !ok {
		return nil, repo.ErrNotFound
	}
	return &team, nil
}

func (f *fakeRepo) SetUserActiveStatus(_ context.Context, userID string, isActive bool) (*model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	user, ok := f.users[userID]
	if !ok {
		return nil, repo.ErrNotFound
	}
	user.IsActive = isActive
	f.users[userID] = user
	return &user, nil
}

func (f *fakeRepo) GetUserByID(_ context.Context, userID string) (*model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	user, ok := f.users[userID]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := user
	return &cp, nil
}

func (f *fakeRepo) ListTeamMembers(_ context.Context, teamName string) ([]model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var users []model.User
	for _, u := range f.users {
		if u.TeamName == teamName {
			users = append(users, u)
		}
	}
	return users, nil
}

func (f *fakeRepo) CreatePR(_ context.Context, pr *model.PullRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.prs[pr.ID]; exists {
		return repo.ErrAlreadyExists
	}

	_, ok := f.users[pr.AuthorID]
	if !ok {
		return repo.ErrNotFound
	}

	now := time.Now()
	pr.Status = model.PROpen
	pr.CreatedAt = &now

	f.prs[pr.ID] = &model.PullRequest{
		ID:                pr.ID,
		Name:              pr.Name,
		AuthorID:          pr.AuthorID,
		Status:            pr.Status,
		AssignedReviewers: pr.AssignedReviewers,
		CreatedAt:         pr.CreatedAt,
	}
	return nil
}

func (f *fakeRepo) GetPRByID(_ context.Context, prID string) (*model.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	pr, ok := f.prs[prID]
	if !ok {
		return nil, repo.ErrNotFound
	}
	clone := *pr
	return &clone, nil
}

func (f *fakeRepo) GetPRByIDForUpdate(ctx context.Context, prID string) (*model.PullRequest, error) {
	return f.GetPRByID(ctx, prID)
}

func (f *fakeRepo) UpdatePR(_ context.Context, pr *model.PullRequest) (*model.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.prs[pr.ID]
	if !ok {
		return nil, repo.ErrNotFound
	}
	current.Status = pr.Status
	current.MergedAt = pr.MergedAt
	current.AssignedReviewers = append([]string(nil), pr.AssignedReviewers...)
	cp := *current
	return &cp, nil
}

func (f *fakeRepo) GetPRsByReviewer(_ context.Context, userID string) ([]model.PullRequestShort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var prs []model.PullRequestShort
	for _, pr := range f.prs {
		for _, r := range pr.AssignedReviewers {
			if r == userID {
				prs = append(prs, model.PullRequestShort{
					ID:       pr.ID,
					Name:     pr.Name,
					AuthorID: pr.AuthorID,
					Status:   pr.Status,
				})
				break
			}
		}
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].ID < prs[j].ID })
	return prs, nil
}

// --- Хелперы для HTTP тестов.

func newTestServer() *httptest.Server {
	fake := newFakeRepo()
	svc := service.NewService(fake)
	h := NewHandler(svc)
	return httptest.NewServer(h.SetupRouter())
}

func doJSON(t *testing.T, client *http.Client, method, url string, body interface{}) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequest(method, url, &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() {
		// Drain the body to allow re-use of the connection.
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(b))
	}()
	var m map[string]any
	if resp.Body != nil {
		_ = json.NewDecoder(resp.Body).Decode(&m)
	}
	return resp, m
}

// --- Тесты.

func TestTeamLifecycle(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	client := srv.Client()

	resp, _ := doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "backend",
		"members": []map[string]any{
			{"user_id": "u1", "username": "alice", "is_active": true},
			{"user_id": "u2", "username": "bob", "is_active": true},
		},
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp, data := doJSON(t, client, http.MethodGet, srv.URL+"/team/get?team_name=backend", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	members, ok := data["members"].([]any)
	require.True(t, ok)
	require.Len(t, members, 2)
}

func TestCreateAndMergePR(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	// Создаем команду с 3 участниками.
	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "platform",
		"members": []map[string]any{
			{"user_id": "u1", "username": "alice", "is_active": true},
			{"user_id": "u2", "username": "bob", "is_active": true},
			{"user_id": "u3", "username": "carol", "is_active": true},
		},
	})

	resp, data := doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/create", map[string]any{
		"pull_request_id":   "pr-1",
		"pull_request_name": "add search",
		"author_id":         "u1",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	pr := data["pr"].(map[string]any)
	reviewers := pr["assigned_reviewers"].([]any)
	require.Len(t, reviewers, 2)

	// Идемпотентный merge.
	resp, data = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/merge", map[string]any{
		"pull_request_id": "pr-1",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	pr = data["pr"].(map[string]any)
	require.Equal(t, "MERGED", pr["status"])

	resp, data = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/merge", map[string]any{
		"pull_request_id": "pr-1",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	pr = data["pr"].(map[string]any)
	require.Equal(t, "MERGED", pr["status"])
}

func TestReassignFlow(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "core",
		"members": []map[string]any{
			{"user_id": "u1", "username": "alice", "is_active": true},
			{"user_id": "u2", "username": "bob", "is_active": true},
			{"user_id": "u3", "username": "carol", "is_active": true},
			{"user_id": "u4", "username": "dan", "is_active": true},
		},
	})

	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/create", map[string]any{
		"pull_request_id":   "pr-2",
		"pull_request_name": "add metrics",
		"author_id":         "u1",
	})

	// Переназначаем одного ревьюера.
	resp, data := doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/reassign", map[string]any{
		"pull_request_id": "pr-2",
		"old_user_id":     "u2",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	replacedBy := data["replaced_by"].(string)
	require.NotEqual(t, "u2", replacedBy)
	require.NotEqual(t, "u1", replacedBy)

	// После merge переназначение запрещено.
	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/merge", map[string]any{
		"pull_request_id": "pr-2",
	})

	resp, data = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/reassign", map[string]any{
		"pull_request_id": "pr-2",
		"old_reviewer_id": "u3", // legacy поле тоже поддерживается
	})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	errPayload := data["error"].(map[string]any)
	require.Equal(t, "PR_MERGED", errPayload["code"])
}

func TestReassignNoCandidateAndNotAssigned(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "small",
		"members": []map[string]any{
			{"user_id": "u1", "username": "alice", "is_active": true},
			{"user_id": "u2", "username": "bob", "is_active": true},
		},
	})
	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/create", map[string]any{
		"pull_request_id":   "pr-no-candidate",
		"pull_request_name": "feat",
		"author_id":         "u1",
	})

	resp, data := doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/reassign", map[string]any{
		"pull_request_id": "pr-no-candidate",
		"old_user_id":     "u2",
	})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, "NO_CANDIDATE", data["error"].(map[string]any)["code"])

	resp, data = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/reassign", map[string]any{
		"pull_request_id": "pr-no-candidate",
		"old_user_id":     "ghost",
	})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "NOT_FOUND", data["error"].(map[string]any)["code"])
}

func TestHealth(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/health")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSetUserActivityNotFound(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	resp, data := doJSON(t, client, http.MethodPost, srv.URL+"/users/setIsActive", map[string]any{
		"user_id":   "ghost",
		"is_active": true,
	})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "NOT_FOUND", data["error"].(map[string]any)["code"])
}

func TestGetUserReviews(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	// seed team and PR
	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "devs",
		"members": []map[string]any{
			{"user_id": "u1", "username": "alice", "is_active": true},
			{"user_id": "u2", "username": "bob", "is_active": true},
		},
	})
	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/create", map[string]any{
		"pull_request_id":   "pr-9",
		"pull_request_name": "feat",
		"author_id":         "u1",
	})

	resp, data := doJSON(t, client, http.MethodGet, srv.URL+"/users/getReview?user_id=u2", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	prs := data["pull_requests"].([]any)
	require.NotEmpty(t, prs)

	resp, data = doJSON(t, client, http.MethodGet, srv.URL+"/users/getReview?user_id=ghost", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	prs = data["pull_requests"].([]any)
	require.Empty(t, prs)
}

func TestRespondErrorMapping(t *testing.T) {
	rec := httptest.NewRecorder()
	respondError(rec, model.ErrPRExists)
	require.Equal(t, http.StatusConflict, rec.Code)

	rec = httptest.NewRecorder()
	respondError(rec, model.ErrBadRequest)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	respondError(rec, model.ErrTeamExists)
	require.Equal(t, http.StatusConflict, rec.Code)

	rec = httptest.NewRecorder()
	respondError(rec, model.ErrPRMerged)
	require.Equal(t, http.StatusConflict, rec.Code)

	rec = httptest.NewRecorder()
	respondError(rec, model.ErrNoCandidate)
	require.Equal(t, http.StatusConflict, rec.Code)

	rec = httptest.NewRecorder()
	respondError(rec, errors.New("boom"))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestValidationErrors(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	resp, data := doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "BAD_REQUEST", data["error"].(map[string]any)["code"])

	resp, data = doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "devs",
		"members": []map[string]any{
			{"user_id": "", "username": "alice", "is_active": true},
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "BAD_REQUEST", data["error"].(map[string]any)["code"])

	resp, data = doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "devs",
		"members": []map[string]any{
			{"user_id": "u1", "username": "alice"},
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "BAD_REQUEST", data["error"].(map[string]any)["code"])

	resp, data = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/create", map[string]any{
		"pull_request_id": "",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "BAD_REQUEST", data["error"].(map[string]any)["code"])

	resp, _ = doJSON(t, client, http.MethodGet, srv.URL+"/users/getReview", nil)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, _ = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/merge", map[string]any{})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, _ = doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/reassign", map[string]any{
		"pull_request_id": "",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// respondJSON encode error path
	rec := httptest.NewRecorder()
	respondJSON(rec, http.StatusOK, map[string]any{"ch": make(chan int)})
}

func TestCreateTeamDuplicate(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "x",
	})
	resp, data := doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "x",
	})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, "TEAM_EXISTS", data["error"].(map[string]any)["code"])
}

func TestCreatePRAuthorNotFound(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	resp, _ := doJSON(t, client, http.MethodPost, srv.URL+"/pullRequest/create", map[string]any{
		"pull_request_id":   "pr-x",
		"pull_request_name": "feat",
		"author_id":         "ghost",
	})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestSetUserActivitySuccess(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	_, _ = doJSON(t, client, http.MethodPost, srv.URL+"/team/add", map[string]any{
		"team_name": "act",
		"members": []map[string]any{
			{"user_id": "u1", "username": "a", "is_active": true},
		},
	})

	resp, data := doJSON(t, client, http.MethodPost, srv.URL+"/users/setIsActive", map[string]any{
		"user_id":   "u1",
		"is_active": false,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	user := data["user"].(map[string]any)
	require.Equal(t, false, user["is_active"])
}

func TestGetTeamNotFound(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	resp, data := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/team/get?team_name=nope", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "NOT_FOUND", data["error"].(map[string]any)["code"])
}

func TestInvalidJSON(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	client := srv.Client()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/team/add", bytes.NewBufferString("{invalid"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	// SetIsActive decode error
	req, err = http.NewRequest(http.MethodPost, srv.URL+"/users/setIsActive", bytes.NewBufferString("{invalid"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, data := doJSON(t, client, http.MethodPost, srv.URL+"/users/setIsActive", map[string]any{
		"user_id": "ghost",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "BAD_REQUEST", data["error"].(map[string]any)["code"])
}

func TestMergePRNotFound(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	resp, data := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/pullRequest/merge", map[string]any{
		"pull_request_id": "unknown",
	})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "NOT_FOUND", data["error"].(map[string]any)["code"])
}

func TestReassignNotFoundPR(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()
	resp, data := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/pullRequest/reassign", map[string]any{
		"pull_request_id": "missing",
		"old_user_id":     "u1",
	})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "NOT_FOUND", data["error"].(map[string]any)["code"])
}
