package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/trainee/review-service/internal/handler"
	repo "github.com/trainee/review-service/internal/repository"
	"github.com/trainee/review-service/internal/service"
	"syscall"
)

var exit = os.Exit

type Config struct {
	DSN      string
	Port     string
	Listener net.Listener
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, loadConfig()); err != nil {
		log.Printf("server failed: %v", err)
		exit(1)
	}
}

func loadConfig() Config {
	port := os.Getenv("APP_PORT")
	if port == "" {
		port = "8080"
	}
	return Config{
		DSN:  os.Getenv("DB_DSN"),
		Port: port,
	}
}

func run(ctx context.Context, cfg Config) error {
	if cfg.DSN == "" {
		return errors.New("DB_DSN environment variable is required")
	}
	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	// Инициализация пула подключений к БД
	// Для продакшена здесь стоит добавить настройку параметров пула (MaxConns и т.д.)
	dbPool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return err
	}
	defer dbPool.Close()

	// Проверка подключения
	if err := dbPool.Ping(ctx); err != nil {
		return err
	}
	slog.Info("Database connected successfully")

	// Примечание: Миграции обрабатываются отдельным контейнером 'migrate' в docker-compose.

	// Внедрение зависимостей (Dependency Injection)
	repository := repo.NewPostgresRepository(dbPool)
	svc := service.NewService(repository)
	hdlr := handler.NewHandler(svc)
	router := hdlr.SetupRouter()

	// Конфигурация HTTP сервера
	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln := cfg.Listener
	if ln == nil {
		ln, err = net.Listen("tcp", ":"+cfg.Port)
		if err != nil {
			return err
		}
	}

	// Запуск сервера в отдельной горутине
	go func() {
		slog.Info("Starting server", "addr", ln.Addr().String())
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Graceful Shutdown
	<-ctx.Done()
	slog.Info("Shutting down server...")

	// Даем 10 секунд на завершение текущих запросов
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}

	slog.Info("Server exiting")
	return nil
}
