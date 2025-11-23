package repository

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"

	"github.com/trainee/review-service/internal/model"
)

func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Skipf("docker not available: %v", err)
	}

	runOpts := &dockertest.RunOptions{
		Repository: "postgres",
		Tag:        "15-alpine",
		Env: []string{
			"POSTGRES_PASSWORD=pass",
			"POSTGRES_USER=user",
			"POSTGRES_DB=testdb",
		},
	}
	resource, err := pool.RunWithOptions(runOpts, func(hc *docker.HostConfig) {
		hc.AutoRemove = true
		hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}

	dsn := fmt.Sprintf("postgres://user:pass@localhost:%s/testdb?sslmode=disable", resource.GetPort("5432/tcp"))

	var db *pgxpool.Pool
	pool.MaxWait = 60 * time.Second
	err = pool.Retry(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		db, err = pgxpool.New(ctx, dsn)
		if err != nil {
			return err
		}
		return db.Ping(ctx)
	})
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}

	initSchema := []string{
		`CREATE TABLE teams (team_name TEXT PRIMARY KEY);`,
		`CREATE TABLE users (
			user_id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			team_name TEXT NOT NULL REFERENCES teams(team_name) ON DELETE RESTRICT,
			is_active BOOLEAN NOT NULL
		);`,
		`CREATE TYPE pr_status AS ENUM ('OPEN', 'MERGED');`,
		`CREATE TABLE pull_requests (
			pull_request_id TEXT PRIMARY KEY,
			pull_request_name TEXT NOT NULL,
			author_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
			status pr_status NOT NULL DEFAULT 'OPEN',
			assigned_reviewers TEXT[] NOT NULL DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			merged_at TIMESTAMPTZ
		);`,
	}

	for _, stmt := range initSchema {
		_, err := db.Exec(context.Background(), stmt)
		require.NoError(t, err)
	}

	cleanup := func() {
		db.Close()
		if err := pool.Purge(resource); err != nil {
			fmt.Fprintf(os.Stderr, "failed to purge container: %v\n", err)
		}
	}
	return db, cleanup
}

func TestPostgresRepository_Workflow(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPostgresRepository(db)
	ctx := context.Background()

	team := model.Team{
		TeamName: "backend",
		Members: []model.TeamMember{
			{UserID: "u1", Username: "alice", IsActive: true},
			{UserID: "u2", Username: "bob", IsActive: true},
			{UserID: "u3", Username: "carol", IsActive: true},
			{UserID: "u4", Username: "dave", IsActive: true},
			{UserID: "u5", Username: "erin", IsActive: false},
		},
	}
	require.NoError(t, repo.CreateTeamTx(ctx, team))

	// Получение команды.
	stored, err := repo.GetTeam(ctx, team.TeamName)
	require.NoError(t, err)
	require.Len(t, stored.Members, 5)

	// Обновление активности пользователя.
	updatedUser, err := repo.SetUserActiveStatus(ctx, "u2", false)
	require.NoError(t, err)
	require.False(t, updatedUser.IsActive)
	// Восстанавливаем активность, чтобы не влиять на выбор ревьюеров позже.
	_, err = repo.SetUserActiveStatus(ctx, "u2", true)
	require.NoError(t, err)

	// Дубликат.
	err = repo.CreateTeamTx(ctx, team)
	require.ErrorIs(t, err, ErrAlreadyExists)

	// Создание PR сохраняет данные и выставляет created_at.
	pr := &model.PullRequest{ID: "pr1", Name: "feat", AuthorID: "u1", Status: model.PROpen}
	require.NoError(t, repo.CreatePR(ctx, pr))
	require.NotNil(t, pr.CreatedAt)

	// Merge через UpdatePR.
	pr.Status = model.PRMerged
	now := time.Now()
	pr.MergedAt = &now
	merged, err := repo.UpdatePR(ctx, pr)
	require.NoError(t, err)
	require.Equal(t, model.PRMerged, merged.Status)
	require.NotNil(t, merged.MergedAt)

	// Повторный update сохраняет merged_at.
	mergedAgain, err := repo.UpdatePR(ctx, merged)
	require.NoError(t, err)
	require.WithinDuration(t, *merged.MergedAt, *mergedAgain.MergedAt, time.Second)

	// Создаем новый PR для проверки обновления ревьюеров.
	pr2 := &model.PullRequest{ID: "pr2", Name: "bugfix", AuthorID: "u1", Status: model.PROpen, AssignedReviewers: []string{"u2", "u3"}}
	require.NoError(t, repo.CreatePR(ctx, pr2))
	pr2.AssignedReviewers[0] = "u4"
	updated, err := repo.UpdatePR(ctx, pr2)
	require.NoError(t, err)
	require.Contains(t, updated.AssignedReviewers, "u4")

	// Поиск PR по ревьюеру.
	prs, err := repo.GetPRsByReviewer(ctx, "u4")
	require.NoError(t, err)
	require.NotEmpty(t, prs)

	// SetUserActiveStatus для несуществующего пользователя -> ErrNotFound
	_, err = repo.SetUserActiveStatus(ctx, "unknown", true)
	require.ErrorIs(t, err, ErrNotFound)

	// GetTeam not found
	_, err = repo.GetTeam(ctx, "nope")
	require.ErrorIs(t, err, ErrNotFound)
}
