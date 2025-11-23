package model

import (
	"errors"
	"time"
)

// Доменные ошибки, соответствующие кодам в OpenAPI.
var (
	ErrNotFound    = errors.New("resource not found")
	ErrTeamExists  = errors.New("team_name already exists")
	ErrPRExists    = errors.New("PR id already exists")
	ErrPRMerged    = errors.New("cannot reassign on merged PR")
	ErrNotAssigned = errors.New("reviewer is not assigned to this PR")
	ErrNoCandidate = errors.New("no active replacement candidate in team")
	ErrBadRequest  = errors.New("invalid request payload or parameters")
	ErrInternal    = errors.New("internal error")
)

type PRStatus string

const (
	PROpen   PRStatus = "OPEN"
	PRMerged PRStatus = "MERGED"
)

// Структуры, соответствующие OpenAPI Schemas
type TeamMember struct {
	UserID   string `json:"user_id" db:"user_id"`
	Username string `json:"username" db:"username"`
	IsActive bool   `json:"is_active" db:"is_active"`
}

type Team struct {
	TeamName string       `json:"team_name"`
	Members  []TeamMember `json:"members"`
}

type User struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	TeamName string `json:"team_name"`
	IsActive bool   `json:"is_active"`
}

type PullRequest struct {
	ID                string     `json:"pull_request_id"`
	Name              string     `json:"pull_request_name"`
	AuthorID          string     `json:"author_id"`
	Status            PRStatus   `json:"status"`
	AssignedReviewers []string   `json:"assigned_reviewers"`
	CreatedAt         *time.Time `json:"createdAt,omitempty"`
	MergedAt          *time.Time `json:"mergedAt,omitempty"`
}

type PullRequestShort struct {
	ID       string   `json:"pull_request_id" db:"pull_request_id"`
	Name     string   `json:"pull_request_name" db:"pull_request_name"`
	AuthorID string   `json:"author_id" db:"author_id"`
	Status   PRStatus `json:"status" db:"status"`
}
