package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	p := write(t, `
[server]
port = 9001
api_token = 0123456789abcdef;#x

[database]
dbname = scanstore
user = app
password = p@ss:w/rd #1
sslmode = disable

[limits]
max_upload_mb = 1.5
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:9001" || c.APIToken != "0123456789abcdef;#x" {
		t.Fatalf("server: %+v", c)
	}
	if c.StoreDir != filepath.Join(filepath.Dir(p), "store") {
		t.Fatalf("store dir = %s", c.StoreDir)
	}
	if c.MaxUploadBytes != 1572864 || c.MaxMetaBytes != 256*1024 {
		t.Fatalf("limits: %d %d", c.MaxUploadBytes, c.MaxMetaBytes)
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.Host != "localhost:5432" || u.Path != "/scanstore" || u.User.Username() != "app" ||
		pw != "p@ss:w/rd #1" || u.Query().Get("sslmode") != "disable" {
		t.Fatalf("dsn = %s", c.DatabaseURL)
	}
}

func TestLoadErrors(t *testing.T) {
	for _, c := range []struct{ ini, want string }{
		{"[server]\napi_token = short\n[database]\ndbname=x", "api_token"},
		{"[server]\napi_token = 0123456789abcdef\n", "dbname"},
	} {
		if _, err := Load(write(t, c.ini)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ожидалась ошибка про %s, получено %v", c.want, err)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.ini")); err == nil {
		t.Error("нет ошибки для отсутствующего файла")
	}
}
