// Package config читает настройки сервера из config.ini.
package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"
)

// MinTokenLen - минимальная длина API-токена: с коротким токеном сервер не стартует.
const MinTokenLen = 16

type Config struct {
	Addr     string // host:port для net/http
	APIToken string

	StoreDir string // абсолютный путь к папке хранилища картинок

	DatabaseURL     string // строка подключения pgx (URL или key=value)
	DBMaxConns      int32
	MigrateOnStart  bool
	MaxUploadBytes  int64
	MaxMetaBytes    int64
	MaxImagePixels  int64
	MaxParallelJPEG int // одновременных декодирований JPEG; 0 - по числу ядер
}

// Load читает ini-файл. Относительный store.dir считается от папки, где лежит файл конфигурации.
func Load(path string) (*Config, error) {
	// Комментарии - только отдельной строкой: ; и # могут встречаться в паролях и токенах.
	f, err := ini.LoadSources(ini.LoadOptions{Insensitive: true, IgnoreInlineComment: true}, path)
	if err != nil {
		return nil, fmt.Errorf("чтение %s: %w", path, err)
	}
	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}

	srv := f.Section("server")
	db := f.Section("database")
	lim := f.Section("limits")
	c := &Config{
		Addr:     net.JoinHostPort(srv.Key("host").MustString("127.0.0.1"), strconv.Itoa(srv.Key("port").MustInt(8000))),
		APIToken: strings.TrimSpace(srv.Key("api_token").String()),

		StoreDir: f.Section("store").Key("dir").MustString("store"),

		DBMaxConns:     int32(db.Key("max_conns").MustInt(10)),
		MigrateOnStart: db.Key("migrate").MustBool(true),

		MaxUploadBytes:  int64(lim.Key("max_upload_mb").MustFloat64(20) * 1024 * 1024),
		MaxMetaBytes:    int64(lim.Key("max_meta_kb").MustFloat64(256) * 1024),
		MaxImagePixels:  int64(lim.Key("max_image_mpix").MustFloat64(80) * 1_000_000),
		MaxParallelJPEG: lim.Key("max_parallel_decode").MustInt(0),
	}
	if !filepath.IsAbs(c.StoreDir) {
		c.StoreDir = filepath.Join(baseDir, c.StoreDir)
	}
	if c.DatabaseURL, err = databaseURL(db); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// databaseURL: явный dsn имеет приоритет, иначе URL собирается из отдельных полей
// (url.UserPassword экранирует спецсимволы в пароле).
func databaseURL(s *ini.Section) (string, error) {
	if dsn := strings.TrimSpace(s.Key("dsn").String()); dsn != "" {
		return dsn, nil
	}
	name := s.Key("dbname").String()
	if name == "" {
		return "", fmt.Errorf("[database]: задайте dsn или dbname/user/password")
	}
	u := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(s.Key("host").MustString("localhost"), strconv.Itoa(s.Key("port").MustInt(5432))),
		Path:   "/" + name,
	}
	if user := s.Key("user").String(); user != "" {
		if pw := s.Key("password").String(); pw != "" {
			u.User = url.UserPassword(user, pw)
		} else {
			u.User = url.User(user)
		}
	}
	q := url.Values{}
	q.Set("sslmode", s.Key("sslmode").MustString("prefer"))
	q.Set("application_name", "scanstore")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Config) validate() error {
	switch {
	case len(c.APIToken) < MinTokenLen:
		return fmt.Errorf("[server] api_token должен быть задан и быть не короче %d символов", MinTokenLen)
	case c.MaxUploadBytes <= 0 || c.MaxMetaBytes <= 0 || c.MaxImagePixels <= 0:
		return fmt.Errorf("[limits]: ограничения должны быть положительными")
	case c.DBMaxConns <= 0:
		return fmt.Errorf("[database] max_conns должен быть положительным")
	}
	return nil
}
