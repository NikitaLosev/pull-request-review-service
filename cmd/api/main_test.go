package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
)

func startTestPostgres(t *testing.T) (dsn string, cleanup func()) {
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
		t.Fatalf("failed to start postgres: %v", err)
	}

	dsn = fmt.Sprintf("postgres://user:pass@localhost:%s/testdb?sslmode=disable", resource.GetPort("5432/tcp"))

	pool.MaxWait = 60 * time.Second
	require.NoError(t, pool.Retry(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		db, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping(ctx)
	}))

	cleanup = func() {
		if err := pool.Purge(resource); err != nil {
			t.Logf("failed to purge postgres: %v", err)
		}
	}
	return dsn, cleanup
}

func TestRunStartsAndStops(t *testing.T) {
	dsn, cleanup := startTestPostgres(t)
	defer cleanup()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{DSN: dsn, Port: "0", Listener: ln})
	}()

	// Даем серверу подняться.
	time.Sleep(300 * time.Millisecond)

	// Проверяем health.
	resp, err := http.Get("http://" + ln.Addr().String() + "/health")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Останавливаем.
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop in time")
	}
}

func TestLoadConfigMissingDSN(t *testing.T) {
	cfg := loadConfig()
	require.Equal(t, "", cfg.DSN)
}

func TestRunRequiresDSN(t *testing.T) {
	err := run(context.Background(), Config{DSN: ""})
	require.Error(t, err)
}

func TestMainMissingDSNExits(t *testing.T) {
	t.Setenv("DB_DSN", "")
	t.Setenv("APP_PORT", "0")

	called := make(chan int, 1)
	prevExit := exit
	exit = func(code int) { called <- code }
	defer func() { exit = prevExit }()

	main()

	select {
	case code := <-called:
		require.Equal(t, 1, code)
	case <-time.After(1 * time.Second):
		t.Fatal("exit was not called")
	}
}
