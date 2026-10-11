package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"io/fs"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scanstore-api-gate/internal/api"
	"scanstore-api-gate/internal/filestore"
	"scanstore-api-gate/internal/metastore"
)

const testToken = "test-token-0123456789"

// testServer - настоящие обработчики и файловое хранилище на диске; метаданные в памяти.
func testServer(t *testing.T) (url, storeDir string) {
	t.Helper()
	storeDir = t.TempDir()
	files, err := filestore.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(api.Options{
		APIToken: testToken, MaxUploadBytes: 20 << 20, MaxMetaBytes: 256 << 10, MaxImagePixels: 80_000_000,
	}, files, metastore.NewMemory(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, storeDir
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".jpg") {
			n++
		}
		return nil
	})
	return n
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBulkload(t *testing.T) {
	var jpg bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	img.Set(5, 5, color.RGBA{200, 0, 0, 255})
	if err := jpeg.Encode(&jpg, img, nil); err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	meta := []byte(`{"type":"passport","fields":{"series":"9218","number":"506640"}}`)
	writeFile(t, filepath.Join(src, "a", "one.jpg"), jpg.Bytes())
	writeFile(t, filepath.Join(src, "a", "one.json"), meta)
	// BOM в начале json не должен мешать
	writeFile(t, filepath.Join(src, "a", "b", "c", "two.JPG"), jpg.Bytes())
	writeFile(t, filepath.Join(src, "a", "b", "c", "two.json"), append([]byte("\xef\xbb\xbf"), meta...))
	writeFile(t, filepath.Join(src, "no_json.jpg"), jpg.Bytes())
	writeFile(t, filepath.Join(src, "no_image.json"), meta)
	// битый json - сервер ответит 422
	writeFile(t, filepath.Join(src, "bad.jpg"), jpg.Bytes())
	writeFile(t, filepath.Join(src, "bad.json"), []byte(`{"type":`))

	url, store := testServer(t)
	cfg := config{dir: src, url: url, token: testToken, out: filepath.Join(t.TempDir(), "log.jsonl"), workers: 3}

	s, err := run(context.Background(), cfg, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if s.pairs != 3 || s.ok != 2 || s.failed != 1 || s.byStatus["422"] != 1 {
		t.Fatalf("сводка: %+v", s)
	}
	if len(s.noJSON) != 1 || len(s.noImage) != 1 {
		t.Fatalf("сироты: noJSON=%v noImage=%v", s.noJSON, s.noImage)
	}
	if n := countFiles(t, store); n != 2 {
		t.Fatalf("в хранилище %d картинок, ожидалось 2", n)
	}

	// повторный запуск: загруженное пропускается, отказавший файл пробуется снова
	s, err = run(context.Background(), cfg, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if s.skipped != 2 || s.ok != 0 || s.failed != 1 {
		t.Fatalf("повторный запуск: %+v", s)
	}
	if n := countFiles(t, store); n != 2 {
		t.Fatalf("после повтора в хранилище %d картинок, ожидалось 2", n)
	}

	// неверный токен
	cfg.token = "wrong-token-0123456789"
	cfg.out = filepath.Join(t.TempDir(), "log2.jsonl")
	if s, _ = run(context.Background(), cfg, io.Discard, io.Discard); s.ok != 0 || s.byStatus["401"] != 3 {
		t.Fatalf("неверный токен: %+v", s)
	}

	// -dry-run ничего не отправляет и не создаёт журнал
	cfg.dryRun = true
	cfg.out = filepath.Join(t.TempDir(), "never.jsonl")
	if s, _ = run(context.Background(), cfg, io.Discard, io.Discard); s.pairs != 3 || s.ok != 0 {
		t.Fatalf("dry-run: %+v", s)
	}
	if _, err := os.Stat(cfg.out); err == nil {
		t.Fatal("dry-run создал журнал")
	}
}

// journalGroups читает журнал: путь картинки -> docgroup.
func journalGroups(t *testing.T, path string) map[string]int64 {
	t.Helper()
	recs, err := readLog(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for img, r := range recs {
		out[img] = r.DocGroup
	}
	return out
}

func TestBulkloadGroups(t *testing.T) {
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 20, 20)), nil); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"type":"passport"}`)
	src := t.TempDir()
	add := func(rel string) {
		writeFile(t, filepath.Join(src, filepath.FromSlash(rel)+".jpg"), jpg.Bytes())
		writeFile(t, filepath.Join(src, filepath.FromSlash(rel)+".json"), meta)
	}
	for _, f := range []string{"d1/x1", "d1/x2", "d1/x3", "d2/y1", "d2/y2", "d2/deep/z1", "top"} {
		add(f)
	}

	url, _ := testServer(t)
	cfg := config{dir: src, url: url, token: testToken, out: filepath.Join(t.TempDir(), "log.jsonl"), workers: 3}
	s, err := run(context.Background(), cfg, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if s.ok != 7 || s.failed != 0 {
		t.Fatalf("сводка: %+v", s)
	}
	g := journalGroups(t, cfg.out)
	if g["d1/x1.jpg"] == 0 || g["d1/x1.jpg"] != g["d1/x2.jpg"] || g["d1/x1.jpg"] != g["d1/x3.jpg"] {
		t.Fatalf("файлы папки d1 в разных группах: %v", g)
	}
	if g["d2/y1.jpg"] == 0 || g["d2/y1.jpg"] != g["d2/y2.jpg"] {
		t.Fatalf("файлы папки d2 в разных группах: %v", g)
	}
	// у каждой папки своя группа, вложенная папка - отдельная
	seen := map[int64]string{}
	for _, f := range []string{"d1/x1.jpg", "d2/y1.jpg", "d2/deep/z1.jpg", "top.jpg"} {
		if other, dup := seen[g[f]]; dup {
			t.Fatalf("%s и %s в одной группе %d", f, other, g[f])
		}
		seen[g[f]] = f
	}

	// -dirs N: берутся первые N папок целиком (d1 с тремя файлами и d2/deep с одним)
	lim := cfg
	lim.dirs = 2
	lim.out = filepath.Join(t.TempDir(), "limited.jsonl")
	url2, _ := testServer(t)
	lim.url = url2
	if s, err = run(context.Background(), lim, io.Discard, io.Discard); err != nil || s.ok != 4 || s.failed != 0 {
		t.Fatalf("-dirs 2: %v %+v", err, s)
	}
	lg := journalGroups(t, lim.out)
	if len(lg) != 4 || lg["d1/x1.jpg"] == 0 || lg["d1/x1.jpg"] != lg["d1/x3.jpg"] || lg["d2/deep/z1.jpg"] == 0 {
		t.Fatalf("-dirs 2: %v", lg)
	}

	// продолжение: новый файл в уже загруженной папке попадает в её группу, а не в новую
	add("d1/x4")
	add("d3/w1")
	if s, err = run(context.Background(), cfg, io.Discard, io.Discard); err != nil || s.ok != 2 || s.skipped != 7 {
		t.Fatalf("продолжение: %v %+v", err, s)
	}
	g2 := journalGroups(t, cfg.out)
	if g2["d1/x4.jpg"] != g["d1/x1.jpg"] {
		t.Fatalf("x4 в группе %d, ожидалась %d", g2["d1/x4.jpg"], g["d1/x1.jpg"])
	}
	if _, dup := seen[g2["d3/w1.jpg"]]; dup || g2["d3/w1.jpg"] == 0 {
		t.Fatalf("новая папка d3 должна получить новую группу: %v", g2)
	}
}

// TestRealOutput загружает настоящую папку со сканами во встроенный сервер (метаданные в памяти, файлы на диск
// во временную папку): проверяет, что сервер принимает все реальные файлы. В базу ничего не пишется.
//
//	$env:SCANSTORE_REAL_OUTPUT = "D:\PyProjs\DocScanSort\Output"; go test ./cmd/bulkload -run RealOutput -v
func TestRealOutput(t *testing.T) {
	dir := os.Getenv("SCANSTORE_REAL_OUTPUT")
	if dir == "" {
		t.Skip("SCANSTORE_REAL_OUTPUT не задан")
	}
	url, store := testServer(t)
	var out, errs bytes.Buffer
	cfg := config{dir: dir, url: url, token: testToken, out: filepath.Join(t.TempDir(), "log.jsonl"), workers: 4}
	s, err := run(context.Background(), cfg, &out, &errs)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + out.String())
	if s.failed > 0 {
		t.Errorf("не загружено %d файлов:\n%s", s.failed, errs.String())
	}
	if n := countFiles(t, store); n != s.ok {
		t.Errorf("в хранилище %d картинок, загружено %d", n, s.ok)
	}
}
