package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/trainee/review-service/internal/model"
)

// Ошибки уровня репозитория
var (
	ErrNotFound      = errors.New("repository: not found")
	ErrAlreadyExists = errors.New("repository: already exists")
)

// Repository описывает операции доступа к данным без бизнес-логики.
type Repository interface {
	CreateTeamTx(ctx context.Context, team model.Team) error
	GetTeam(ctx context.Context, teamName string) (*model.Team, error)
	SetUserActiveStatus(ctx context.Context, userID string, isActive bool) (*model.User, error)

	GetUserByID(ctx context.Context, userID string) (*model.User, error)
	ListTeamMembers(ctx context.Context, teamName string) ([]model.User, error)

	CreatePR(ctx context.Context, pr *model.PullRequest) error
	GetPRByID(ctx context.Context, prID string) (*model.PullRequest, error)
	GetPRByIDForUpdate(ctx context.Context, prID string) (*model.PullRequest, error)
	UpdatePR(ctx context.Context, pr *model.PullRequest) (*model.PullRequest, error)

	GetPRsByReviewer(ctx context.Context, userID string) ([]model.PullRequestShort, error)

	WithTransaction(ctx context.Context, fn func(tx TxRepository) error) error
}

// TxRepository используется внутри транзакций (тот же набор методов, кроме управления транзакцией).
type TxRepository interface {
	GetUserByID(ctx context.Context, userID string) (*model.User, error)
	ListTeamMembers(ctx context.Context, teamName string) ([]model.User, error)

	CreatePR(ctx context.Context, pr *model.PullRequest) error
	GetPRByID(ctx context.Context, prID string) (*model.PullRequest, error)
	GetPRByIDForUpdate(ctx context.Context, prID string) (*model.PullRequest, error)
	UpdatePR(ctx context.Context, pr *model.PullRequest) (*model.PullRequest, error)

	GetPRsByReviewer(ctx context.Context, userID string) ([]model.PullRequestShort, error)
}

type PostgresRepository struct {
	pool *pgxpool.Pool
}

// queryable описывает минимальный набор методов, доступных у *pgxpool.Pool и pgx.Tx.
type queryable interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// handleError маппит ошибки PostgreSQL на ошибки репозитория.
func handleError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgerrcode.UniqueViolation:
			return ErrAlreadyExists
		case pgerrcode.ForeignKeyViolation:
			// Если нарушен FK (например, автор не найден), это эквивалентно NotFound для вызывающего кода.
			return ErrNotFound
		}
	}
	return err
}

// --- Хелперы ---

// getUser - хелпер для работы с пулом и транзакциями.
func getUser(ctx context.Context, q queryable, userID string) (*model.User, error) {
	query := `
		SELECT user_id, username, team_name, is_active
		FROM users WHERE user_id = $1
	`
	var user model.User
	err := q.QueryRow(ctx, query, userID).Scan(&user.UserID, &user.Username, &user.TeamName, &user.IsActive)
	if err != nil {
		return nil, handleError(err)
	}
	return &user, nil
}

// --- Teams & Users ---

// CreateTeamTx создает команду и её участников транзакционно.
func (r *PostgresRepository) CreateTeamTx(ctx context.Context, team model.Team) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	// Гарантируем Rollback в случае ошибки
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// 1. Вставка команды
	_, err = tx.Exec(ctx, `INSERT INTO teams (team_name) VALUES ($1)`, team.TeamName)
	if err != nil {
		return handleError(err)
	}

	// 2. Вставка/Обновление пользователей (UPSERT) с использованием pgx.Batch
	if len(team.Members) > 0 {
		batch := &pgx.Batch{}
		query := `
			INSERT INTO users (user_id, username, team_name, is_active)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id) DO UPDATE SET
				username = EXCLUDED.username,
				is_active = EXCLUDED.is_active,
				team_name = EXCLUDED.team_name
		`
		for _, member := range team.Members {
			batch.Queue(query, member.UserID, member.Username, team.TeamName, member.IsActive)
		}

		br := tx.SendBatch(ctx, batch)
		if err := br.Close(); err != nil {
			return fmt.Errorf("batch insert users failed: %w", handleError(err))
		}
	}

	return tx.Commit(ctx)
}

func (r *PostgresRepository) GetTeam(ctx context.Context, teamName string) (*model.Team, error) {
	// Проверка существования команды
	var exists bool
	err := r.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM teams WHERE team_name = $1)", teamName).Scan(&exists)
	if err != nil || !exists {
		if !exists {
			return nil, ErrNotFound
		}
		return nil, err
	}

	// Получение участников
	query := `
		SELECT user_id, username, is_active
		FROM users
		WHERE team_name = $1
		ORDER BY user_id
	`
	rows, err := r.pool.Query(ctx, query, teamName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Используем маппинг по имени структуры для удобства
	members, err := pgx.CollectRows(rows, pgx.RowToStructByName[model.TeamMember])
	if err != nil {
		return nil, err
	}

	return &model.Team{TeamName: teamName, Members: members}, nil
}

func (r *PostgresRepository) SetUserActiveStatus(ctx context.Context, userID string, isActive bool) (*model.User, error) {
	query := `
		UPDATE users SET is_active = $2 WHERE user_id = $1
		RETURNING user_id, username, team_name, is_active
	`
	var user model.User
	err := r.pool.QueryRow(ctx, query, userID, isActive).Scan(&user.UserID, &user.Username, &user.TeamName, &user.IsActive)
	if err != nil {
		return nil, handleError(err)
	}
	return &user, nil
}

func (r *PostgresRepository) GetUserByID(ctx context.Context, userID string) (*model.User, error) {
	return getUser(ctx, r.pool, userID)
}

func (r *PostgresRepository) ListTeamMembers(ctx context.Context, teamName string) ([]model.User, error) {
	query := `
		SELECT user_id, username, team_name, is_active
		FROM users
		WHERE team_name = $1
	`
	rows, err := r.pool.Query(ctx, query, teamName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	members, err := pgx.CollectRows(rows, pgx.RowToStructByName[model.User])
	return members, err
}

// --- Pull Requests ---

// CreatePR добавляет PR с уже подготовленными данными.
func (r *PostgresRepository) CreatePR(ctx context.Context, pr *model.PullRequest) error {
	reviewers := pr.AssignedReviewers
	if reviewers == nil {
		reviewers = []string{}
	}
	query := `
		INSERT INTO pull_requests (pull_request_id, pull_request_name, author_id, status, assigned_reviewers, merged_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at
	`
	err := r.pool.QueryRow(ctx, query, pr.ID, pr.Name, pr.AuthorID, pr.Status, reviewers, pr.MergedAt).Scan(&pr.CreatedAt)
	return handleError(err)
}

func (r *PostgresRepository) GetPRByID(ctx context.Context, prID string) (*model.PullRequest, error) {
	return fetchPR(ctx, r.pool, prID, false)
}

func fetchPR(ctx context.Context, q queryable, prID string, forUpdate bool) (*model.PullRequest, error) {
	suffix := ""
	if forUpdate {
		suffix = " FOR UPDATE"
	}
	query := `
		SELECT pull_request_id, pull_request_name, author_id, status, assigned_reviewers, created_at, merged_at
		FROM pull_requests WHERE pull_request_id = $1` + suffix
	var pr model.PullRequest
	err := q.QueryRow(ctx, query, prID).Scan(
		&pr.ID, &pr.Name, &pr.AuthorID, &pr.Status, &pr.AssignedReviewers, &pr.CreatedAt, &pr.MergedAt,
	)
	if err != nil {
		return nil, handleError(err)
	}
	if pr.AssignedReviewers == nil {
		pr.AssignedReviewers = []string{}
	}
	return &pr, nil
}

func (r *PostgresRepository) GetPRByIDForUpdate(ctx context.Context, prID string) (*model.PullRequest, error) {
	return fetchPR(ctx, r.pool, prID, true)
}

// UpdatePR обновляет статус/ревьюеров и возвращает текущее состояние.
func (r *PostgresRepository) UpdatePR(ctx context.Context, pr *model.PullRequest) (*model.PullRequest, error) {
	reviewers := pr.AssignedReviewers
	if reviewers == nil {
		reviewers = []string{}
	}
	query := `
		UPDATE pull_requests
		SET status = $2,
		    assigned_reviewers = $3,
		    merged_at = $4
		WHERE pull_request_id = $1
		RETURNING pull_request_id, pull_request_name, author_id, status, assigned_reviewers, created_at, merged_at
	`
	var updated model.PullRequest
	err := r.pool.QueryRow(ctx, query, pr.ID, pr.Status, reviewers, pr.MergedAt).Scan(
		&updated.ID, &updated.Name, &updated.AuthorID, &updated.Status, &updated.AssignedReviewers, &updated.CreatedAt, &updated.MergedAt,
	)
	if err != nil {
		return nil, handleError(err)
	}
	if updated.AssignedReviewers == nil {
		updated.AssignedReviewers = []string{}
	}
	return &updated, nil
}

// GetPRsByReviewer находит все PR, назначенные пользователю.
func (r *PostgresRepository) GetPRsByReviewer(ctx context.Context, userID string) ([]model.PullRequestShort, error) {
	query := `
		SELECT pull_request_id, pull_request_name, author_id, status
		FROM pull_requests
		WHERE assigned_reviewers @> ARRAY[$1]::TEXT[]
		ORDER BY created_at DESC
	`
	rows, err := r.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	prs, err := pgx.CollectRows(rows, pgx.RowToStructByName[model.PullRequestShort])
	return prs, err
}

func (r *PostgresRepository) WithTransaction(ctx context.Context, fn func(tx TxRepository) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	wrapper := &txRepository{tx: tx}

	if err := fn(wrapper); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

type txRepository struct {
	tx pgx.Tx
}

func (t *txRepository) GetUserByID(ctx context.Context, userID string) (*model.User, error) {
	return getUser(ctx, t.tx, userID)
}

func (t *txRepository) ListTeamMembers(ctx context.Context, teamName string) ([]model.User, error) {
	query := `
		SELECT user_id, username, team_name, is_active
		FROM users
		WHERE team_name = $1
	`
	rows, err := t.tx.Query(ctx, query, teamName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	members, err := pgx.CollectRows(rows, pgx.RowToStructByName[model.User])
	return members, err
}

func (t *txRepository) CreatePR(ctx context.Context, pr *model.PullRequest) error {
	reviewers := pr.AssignedReviewers
	if reviewers == nil {
		reviewers = []string{}
	}
	query := `
		INSERT INTO pull_requests (pull_request_id, pull_request_name, author_id, status, assigned_reviewers, merged_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at
	`
	err := t.tx.QueryRow(ctx, query, pr.ID, pr.Name, pr.AuthorID, pr.Status, reviewers, pr.MergedAt).Scan(&pr.CreatedAt)
	return handleError(err)
}

func (t *txRepository) GetPRByID(ctx context.Context, prID string) (*model.PullRequest, error) {
	return fetchPR(ctx, t.tx, prID, false)
}

func (t *txRepository) GetPRByIDForUpdate(ctx context.Context, prID string) (*model.PullRequest, error) {
	return fetchPR(ctx, t.tx, prID, true)
}

func (t *txRepository) UpdatePR(ctx context.Context, pr *model.PullRequest) (*model.PullRequest, error) {
	reviewers := pr.AssignedReviewers
	if reviewers == nil {
		reviewers = []string{}
	}
	query := `
		UPDATE pull_requests
		SET status = $2,
		    assigned_reviewers = $3,
		    merged_at = $4
		WHERE pull_request_id = $1
		RETURNING pull_request_id, pull_request_name, author_id, status, assigned_reviewers, created_at, merged_at
	`
	var updated model.PullRequest
	err := t.tx.QueryRow(ctx, query, pr.ID, pr.Status, reviewers, pr.MergedAt).Scan(
		&updated.ID, &updated.Name, &updated.AuthorID, &updated.Status, &updated.AssignedReviewers, &updated.CreatedAt, &updated.MergedAt,
	)
	if err != nil {
		return nil, handleError(err)
	}
	if updated.AssignedReviewers == nil {
		updated.AssignedReviewers = []string{}
	}
	return &updated, nil
}

func (t *txRepository) GetPRsByReviewer(ctx context.Context, userID string) ([]model.PullRequestShort, error) {
	query := `
		SELECT pull_request_id, pull_request_name, author_id, status
		FROM pull_requests
		WHERE assigned_reviewers @> ARRAY[$1]::TEXT[]
		ORDER BY created_at DESC
	`
	rows, err := t.tx.Query(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	prs, err := pgx.CollectRows(rows, pgx.RowToStructByName[model.PullRequestShort])
	return prs, err
}
