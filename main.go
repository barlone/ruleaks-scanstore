// scanstore - REST API хранилища сканов документов: картинки (JPEG) на файловой системе,
// метаинформация в PostgreSQL. Замена прототипа DocScanSort/docstoreapi.py.
//
//	scanstore [-config config.ini]
//	scanstore -gen-token
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"scanstore-api-gate/internal/api"
	"scanstore-api-gate/internal/config"
	"scanstore-api-gate/internal/filestore"
	"scanstore-api-gate/internal/metastore"
)

func main() {
	configPath := flag.String("config", "config.ini", "путь к файлу конфигурации")
	genToken := flag.Bool("gen-token", false, "напечатать случайный api_token и выйти")
	flag.Parse()

	if *genToken {
		b := make([]byte, 32)
		_, _ = rand.Read(b) // crypto/rand.Read не возвращает ошибок
		fmt.Println(base64.RawURLEncoding.EncodeToString(b))
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*configPath, log); err != nil {
		log.Error("сервер остановлен с ошибкой", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	files, err := filestore.Open(cfg.StoreDir)
	if err != nil {
		return fmt.Errorf("папка хранилища %s: %w", cfg.StoreDir, err)
	}

	// Ctrl+C и Ctrl+Break на Windows приходят как os.Interrupt
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	db, err := metastore.OpenPostgres(dbCtx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.MigrateOnStart)
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: api.New(api.Options{
			APIToken:        cfg.APIToken,
			MaxUploadBytes:  cfg.MaxUploadBytes,
			MaxMetaBytes:    cfg.MaxMetaBytes,
			MaxImagePixels:  cfg.MaxImagePixels,
			MaxParallelJPEG: cfg.MaxParallelJPEG,
		}, files, db, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute, // загрузка 20 МБ по медленному каналу
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("сервер запущен", "addr", cfg.Addr, "store", cfg.StoreDir)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("остановка: дожидаемся текущих запросов")
	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
