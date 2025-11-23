BEGIN;

-- 1. Teams
CREATE TABLE IF NOT EXISTS teams (
    team_name TEXT PRIMARY KEY
);

-- 2. Users
CREATE TABLE IF NOT EXISTS users (
    user_id TEXT PRIMARY KEY,
    username TEXT NOT NULL,
    -- Используем ON DELETE RESTRICT, чтобы предотвратить удаление команды, если в ней есть пользователи
    team_name TEXT NOT NULL REFERENCES teams(team_name) ON DELETE RESTRICT,
    is_active BOOLEAN NOT NULL
);

-- Индекс для быстрого поиска кандидатов на ревью (по команде и активности)
CREATE INDEX IF NOT EXISTS idx_users_team_active ON users(team_name, is_active);

-- 3. Pull Requests
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'pr_status') THEN
        CREATE TYPE pr_status AS ENUM ('OPEN', 'MERGED');
    END IF;
END$$;

CREATE TABLE IF NOT EXISTS pull_requests (
    pull_request_id TEXT PRIMARY KEY,
    pull_request_name TEXT NOT NULL,
    author_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
    status pr_status NOT NULL DEFAULT 'OPEN',
    -- Храним ревьюеров как массив user_id (максимум 2).
    assigned_reviewers TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    merged_at TIMESTAMPTZ
);

-- GIN индекс для эффективного поиска внутри массива (поиск PR по ревьюеру)
CREATE INDEX IF NOT EXISTS idx_pr_reviewers_gin ON pull_requests USING GIN(assigned_reviewers);

COMMIT;