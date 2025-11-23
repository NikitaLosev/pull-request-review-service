package service

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trainee/review-service/internal/model"
	repo "github.com/trainee/review-service/internal/repository"
)

// in-memory fake, детерминированные результаты для тестов сервиса.
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
	if _, ok := f.teams[team.TeamName]; ok {
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
	tm, ok := f.teams[teamName]
	if !ok {
		return nil, repo.ErrNotFound
	}
	return &tm, nil
}

func (f *fakeRepo) SetUserActiveStatus(_ context.Context, userID string, isActive bool) (*model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[userID]
	if !ok {
		return nil, repo.ErrNotFound
	}
	u.IsActive = isActive
	f.users[userID] = u
	return &u, nil
}

func (f *fakeRepo) GetUserByID(_ context.Context, userID string) (*model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[userID]
	if !ok {
		return nil, repo.ErrNotFound
	}
	copy := u
	return &copy, nil
}

func (f *fakeRepo) ListTeamMembers(_ context.Context, teamName string) ([]model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var members []model.User
	for _, u := range f.users {
		if u.TeamName == teamName {
			members = append(members, u)
		}
	}
	return members, nil
}

func (f *fakeRepo) CreatePR(_ context.Context, pr *model.PullRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.prs[pr.ID]; ok {
		return repo.ErrAlreadyExists
	}
	_, ok := f.users[pr.AuthorID]
	if !ok {
		return repo.ErrNotFound
	}
	now := time.Now()
	pr.CreatedAt = &now
	pr.Status = model.PROpen
	f.prs[pr.ID] = pr
	return nil
}

func (f *fakeRepo) GetPRByID(_ context.Context, prID string) (*model.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[prID]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *pr
	return &cp, nil
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
	current.AssignedReviewers = append([]string(nil), pr.AssignedReviewers...)
	current.MergedAt = pr.MergedAt
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
	sort.Slice(prs, func(i, j int) bool {
		return prs[i].ID < prs[j].ID
	})
	return prs, nil
}

// --- Тесты сервиса ---

func prepareService() (*Service, *fakeRepo) {
	f := newFakeRepo()
	s := NewService(f)
	return s, f
}

func seedTeam(f *fakeRepo, name string, active bool, ids ...string) {
	members := make([]model.TeamMember, 0, len(ids))
	for i, id := range ids {
		members = append(members, model.TeamMember{
			UserID:   id,
			Username: "user" + strconv.Itoa(i),
			IsActive: active,
		})
	}
	_ = f.CreateTeamTx(context.Background(), model.Team{
		TeamName: name,
		Members:  members,
	})
}

func TestCreateTeamAlreadyExists(t *testing.T) {
	svc, f := prepareService()
	seedTeam(f, "t1", true, "u1")

	err := svc.CreateTeam(context.Background(), model.Team{TeamName: "t1"})
	require.ErrorIs(t, err, model.ErrTeamExists)
}

func TestGetTeamNotFound(t *testing.T) {
	svc, _ := prepareService()
	_, err := svc.GetTeam(context.Background(), "missing")
	require.ErrorIs(t, err, model.ErrNotFound)
}

func TestCreatePullRequestAssignsReviewers(t *testing.T) {
	svc, f := prepareService()
	seedTeam(f, "backend", true, "u1", "u2", "u3")

	pr, err := svc.CreatePullRequest(context.Background(), "pr1", "feat", "u1")
	require.NoError(t, err)
	require.Equal(t, model.PROpen, pr.Status)
	require.Len(t, pr.AssignedReviewers, 2)
	require.NotContains(t, pr.AssignedReviewers, "u1")
}

func TestCreatePullRequestNoAuthor(t *testing.T) {
	svc, _ := prepareService()
	_, err := svc.CreatePullRequest(context.Background(), "pr1", "feat", "unknown")
	require.ErrorIs(t, err, model.ErrNotFound)
}

func TestMergeIdempotent(t *testing.T) {
	svc, f := prepareService()
	seedTeam(f, "backend", true, "u1", "u2")
	_, _ = svc.CreatePullRequest(context.Background(), "pr1", "feat", "u1")

	pr, err := svc.MergePullRequest(context.Background(), "pr1")
	require.NoError(t, err)
	require.Equal(t, model.PRMerged, pr.Status)
	firstMerged := pr.MergedAt

	pr, err = svc.MergePullRequest(context.Background(), "pr1")
	require.NoError(t, err)
	require.Equal(t, model.PRMerged, pr.Status)
	require.WithinDuration(t, *firstMerged, *pr.MergedAt, time.Second)
}

func TestReassignValidation(t *testing.T) {
	svc, f := prepareService()
	seedTeam(f, "backend", true, "u1", "u2", "u3", "u4")
	_, _ = svc.CreatePullRequest(context.Background(), "pr1", "feat", "u1")

	// Unknown user -> ErrNotFound
	_, _, err := svc.ReassignReviewer(context.Background(), "pr1", "u9")
	require.ErrorIs(t, err, model.ErrNotFound)

	// Not assigned but exists -> ErrNotAssigned
	pr, err := f.GetPRByID(context.Background(), "pr1")
	require.NoError(t, err)
	unassigned := ""
	for _, candidate := range []string{"u2", "u3", "u4"} {
		if !isReviewerAssigned(pr.AssignedReviewers, candidate) {
			unassigned = candidate
			break
		}
	}
	require.NotEmpty(t, unassigned)
	_, _, err = svc.ReassignReviewer(context.Background(), "pr1", unassigned)
	require.ErrorIs(t, err, model.ErrNotAssigned)

	// Merge -> ErrPRMerged
	_, _ = svc.MergePullRequest(context.Background(), "pr1")
	_, _, err = svc.ReassignReviewer(context.Background(), "pr1", "u2")
	require.ErrorIs(t, err, model.ErrPRMerged)
}

func TestGetPRsByReviewerNotFound(t *testing.T) {
	svc, _ := prepareService()
	prs, err := svc.GetUserReviewPRs(context.Background(), "missing")
	require.NoError(t, err)
	require.Empty(t, prs)
}

func TestSetUserActiveStatusNotFound(t *testing.T) {
	svc, _ := prepareService()
	_, err := svc.SetUserActiveStatus(context.Background(), "ghost", false)
	require.ErrorIs(t, err, model.ErrNotFound)
}

func TestReassignNoCandidate(t *testing.T) {
	svc, f := prepareService()
	seedTeam(f, "backend", true, "u1", "u2")
	_, _ = svc.CreatePullRequest(context.Background(), "pr1", "feat", "u1")

	_, _, err := svc.ReassignReviewer(context.Background(), "pr1", "u2")
	require.ErrorIs(t, err, model.ErrNoCandidate)
}

func TestMergePRNotFound(t *testing.T) {
	svc, _ := prepareService()
	_, err := svc.MergePullRequest(context.Background(), "absent")
	require.ErrorIs(t, err, model.ErrNotFound)
}
