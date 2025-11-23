package service

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/trainee/review-service/internal/model"
	repo "github.com/trainee/review-service/internal/repository"
)

type Service struct {
	repo repo.Repository
}

func NewService(repository repo.Repository) *Service {
	return &Service{repo: repository}
}

// mapError переводит ошибки репозитория в доменные ошибки.
func mapError(err error) error {
	if err == nil {
		return nil
	}

	// Обработка специфичных доменных ошибок, которые могут прийти из сервисной логики
	if errors.Is(err, model.ErrPRMerged) || errors.Is(err, model.ErrNotAssigned) || errors.Is(err, model.ErrNoCandidate) {
		return err
	}

	// Стандартные ошибки репозитория
	if errors.Is(err, repo.ErrNotFound) {
		return model.ErrNotFound
	}
	// ErrAlreadyExists обрабатывается в конкретных методах (CreateTeam, CreatePR)

	// Все остальные ошибки считаются внутренними ошибками сервера
	return err
}

// --- Teams & Users ---

func (s *Service) CreateTeam(ctx context.Context, team model.Team) error {
	err := s.repo.CreateTeamTx(ctx, team)
	if errors.Is(err, repo.ErrAlreadyExists) {
		return model.ErrTeamExists
	}
	return mapError(err)
}

func (s *Service) GetTeam(ctx context.Context, teamName string) (*model.Team, error) {
	team, err := s.repo.GetTeam(ctx, teamName)
	return team, mapError(err)
}

func (s *Service) SetUserActiveStatus(ctx context.Context, userID string, isActive bool) (*model.User, error) {
	user, err := s.repo.SetUserActiveStatus(ctx, userID, isActive)
	return user, mapError(err)
}

func (s *Service) GetUserReviewPRs(ctx context.Context, userID string) ([]model.PullRequestShort, error) {
	prs, err := s.repo.GetPRsByReviewer(ctx, userID)
	return prs, mapError(err)
}

// --- Pull Requests ---

func (s *Service) CreatePullRequest(ctx context.Context, prID, prName, authorID string) (*model.PullRequest, error) {
	pr := &model.PullRequest{
		ID:       prID,
		Name:     prName,
		AuthorID: authorID,
		Status:   model.PROpen,
	}

	err := s.repo.WithTransaction(ctx, func(tx repo.TxRepository) error {
		author, err := tx.GetUserByID(ctx, authorID)
		if err != nil {
			return err
		}

		teamMembers, err := tx.ListTeamMembers(ctx, author.TeamName)
		if err != nil {
			return err
		}

		candidates := activeReviewers(teamMembers, authorID, nil)
		pr.AssignedReviewers = selectRandom(candidates, 2)

		return tx.CreatePR(ctx, pr)
	})

	if errors.Is(err, repo.ErrAlreadyExists) {
		return nil, model.ErrPRExists
	}
	if err != nil {
		return nil, mapError(err)
	}

	return pr, nil
}

func (s *Service) MergePullRequest(ctx context.Context, prID string) (*model.PullRequest, error) {
	var result *model.PullRequest

	err := s.repo.WithTransaction(ctx, func(tx repo.TxRepository) error {
		current, err := tx.GetPRByIDForUpdate(ctx, prID)
		if err != nil {
			return err
		}
		if current.Status == model.PRMerged {
			result = current
			return nil
		}

		now := time.Now()
		current.Status = model.PRMerged
		current.MergedAt = &now

		result, err = tx.UpdatePR(ctx, current)
		return err
	})

	return result, mapError(err)
}

func (s *Service) ReassignReviewer(ctx context.Context, prID, oldUserID string) (*model.PullRequest, string, error) {
	var (
		updated    *model.PullRequest
		replacedBy string
	)

	err := s.repo.WithTransaction(ctx, func(tx repo.TxRepository) error {
		current, err := tx.GetPRByIDForUpdate(ctx, prID)
		if err != nil {
			return err
		}
		if current.Status == model.PRMerged {
			return model.ErrPRMerged
		}

		oldUser, err := tx.GetUserByID(ctx, oldUserID)
		if err != nil {
			return err
		}

		if !isReviewerAssigned(current.AssignedReviewers, oldUserID) {
			return model.ErrNotAssigned
		}

		candidates, err := activeReviewersFromTeam(ctx, tx, oldUser.TeamName, current.AuthorID, oldUserID, current.AssignedReviewers)
		if err != nil {
			return err
		}
		reviewers := selectRandom(candidates, 1)
		if len(reviewers) == 0 {
			return model.ErrNoCandidate
		}
		replacedBy = reviewers[0]

		for i, r := range current.AssignedReviewers {
			if r == oldUserID {
				current.AssignedReviewers[i] = replacedBy
				break
			}
		}

		updated, err = tx.UpdatePR(ctx, current)
		return err
	})

	return updated, replacedBy, mapError(err)
}

// activeReviewers фильтрует активных участников команды, исключая автора и уже назначенных.
func activeReviewers(users []model.User, authorID string, exclude []string) []string {
	excludeSet := make(map[string]struct{}, len(exclude)+1)
	excludeSet[authorID] = struct{}{}
	for _, id := range exclude {
		excludeSet[id] = struct{}{}
	}

	result := make([]string, 0, len(users))
	for _, u := range users {
		if !u.IsActive {
			continue
		}
		if _, skip := excludeSet[u.UserID]; skip {
			continue
		}
		result = append(result, u.UserID)
	}
	return result
}

func activeReviewersFromTeam(ctx context.Context, tx repo.TxRepository, teamName, authorID, oldUserID string, currentReviewers []string) ([]string, error) {
	members, err := tx.ListTeamMembers(ctx, teamName)
	if err != nil {
		return nil, err
	}
	return activeReviewers(members, authorID, append(currentReviewers, oldUserID)), nil
}

func selectRandom(ids []string, limit int) []string {
	if len(ids) <= limit {
		// Возвращаем копию, чтобы не зависеть от исходного слайса.
		return append([]string(nil), ids...)
	}

	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })

	return append([]string(nil), ids[:limit]...)
}

func isReviewerAssigned(reviewers []string, userID string) bool {
	for _, r := range reviewers {
		if r == userID {
			return true
		}
	}
	return false
}
