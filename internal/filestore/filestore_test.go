package filestore

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLayoutByUUID7Time(t *testing.T) {
	s := &Store{root: "root"}
	id := uuid.MustParse("018b2f8e-2a40-7000-8000-000000000000") // 0x018b2f8e2a40 мс = 2023-10-14 18:58:48 UTC
	if got, want := s.Path(id), filepath.Join("root", "2023", "10", id.String()+".jpg"); got != want {
		t.Fatalf("Path = %s, ожидался %s", got, want)
	}
	now, _ := uuid.NewV7()
	if got, want := filepath.Dir(s.Path(now)), filepath.Join("root", time.Now().UTC().Format("2006"), time.Now().UTC().Format("01")); got != want {
		t.Fatalf("каталог %s, ожидался %s", got, want)
	}
	v4 := uuid.MustParse("ab0e8400-e29b-41d4-a716-446655440000")
	if got, want := s.Path(v4), filepath.Join("root", "other", "ab", v4.String()+".jpg"); got != want {
		t.Fatalf("Path(v4) = %s", got)
	}
}

func TestRelAbs(t *testing.T) {
	s := &Store{root: "root"}
	id := uuid.MustParse("018b2f8e-2a40-7000-8000-000000000000")
	rel := Rel(id)
	if rel != "2023/10/"+id.String()+".jpg" {
		t.Fatalf("Rel = %q", rel)
	}
	abs, err := s.Abs(rel)
	if err != nil || abs != s.Path(id) {
		t.Fatalf("Abs = %q, %v; ожидался %q", abs, err, s.Path(id))
	}
	for _, bad := range []string{"", ".", "../x.jpg", "/etc/passwd", "2023/../../x.jpg", `C:\x.jpg`, "a//b.jpg"} {
		if _, err := s.Abs(bad); err == nil {
			t.Errorf("Abs(%q) должен отвергаться", bad)
		}
	}
}

func TestPutRemove(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "tmp", "old.part")
	_ = os.MkdirAll(filepath.Dir(stale), 0o755)
	_ = os.WriteFile(stale, nil, 0o644)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stale, old, old)

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("зависший .part не удалён при старте")
	}

	id, _ := uuid.NewV7()
	data := []byte("jpeg-bytes")
	if err := s.Put(id, data); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s.Path(id)); !bytes.Equal(got, data) {
		t.Fatal("содержимое не совпадает")
	}
	if left, _ := os.ReadDir(filepath.Join(root, "tmp")); len(left) != 0 {
		t.Fatalf("в tmp остались файлы: %v", left)
	}
	if err := s.Remove(id); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(id); err != nil {
		t.Fatal("повторное удаление должно быть без ошибки")
	}
}
