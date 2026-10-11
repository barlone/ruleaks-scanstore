// Package filestore хранит картинки сканов на диске.
//
// Раскладка: <root>/<ГГГГ>/<ММ>/<uuid>.jpg, где год и месяц (UTC) берутся из метки времени в uuid7.
// Путь вычисляется из id при загрузке (Rel) и записывается в БД (scans.file); читается файл уже по значению
// из БД (Abs). Папки по месяцам удобны для инкрементного бэкапа и не разрастаются: в прототипе каталоги
// ab/cd по первым символам uuid7 менялись раз в ~50 дней.
//
// Запись атомарная: <root>/tmp/*.part -> fsync -> rename. Файл неизменяем, перезаписи нет.
package filestore

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	tmpDirName = "tmp"
	partSuffix = ".part"
	// Недописанные временные файлы старше этого возраста удаляются при старте.
	staleTmpAge = time.Hour
)

type Store struct {
	root string
	tmp  string
}

// Open создаёт папки хранилища и удаляет зависшие временные файлы от прошлых запусков.
func Open(root string) (*Store, error) {
	s := &Store{root: root, tmp: filepath.Join(root, tmpDirName)}
	if err := os.MkdirAll(s.tmp, 0o755); err != nil {
		return nil, err
	}
	s.cleanTmp()
	return s, nil
}

func (s *Store) cleanTmp() {
	entries, err := os.ReadDir(s.tmp)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), partSuffix) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleTmpAge {
			_ = os.Remove(filepath.Join(s.tmp, e.Name()))
		}
	}
}

// Rel - путь к картинке относительно корня хранилища, с разделителем "/" на любой ОС.
// Именно он записывается в scans.file.
func Rel(id uuid.UUID) string {
	return path.Join(relDir(id), id.String()+".jpg")
}

// Path - полный путь к картинке скана (существование файла не проверяет).
func (s *Store) Path(id uuid.UUID) string {
	return filepath.Join(s.root, filepath.FromSlash(Rel(id)))
}

// Abs превращает путь из scans.file в полный путь. Путь из БД не должен выходить за корень хранилища.
func (s *Store) Abs(rel string) (string, error) {
	// fs.ValidPath знает только "/"; "\" и ":" на Windows дают выход за корень (..\x, C:x)
	if !fs.ValidPath(rel) || rel == "." || strings.ContainsAny(rel, `\:`) {
		return "", fmt.Errorf("недопустимый путь к файлу скана %q", rel)
	}
	return filepath.Join(s.root, filepath.FromSlash(rel)), nil
}

// relDir: для uuid7 - ГГГГ/ММ по метке времени; другие версии сервер не выдаёт,
// но на всякий случай они раскладываются по первому байту.
func relDir(id uuid.UUID) string {
	if id.Version() != 7 {
		return path.Join("other", id.String()[:2])
	}
	var b [8]byte
	copy(b[2:], id[:6]) // первые 48 бит uuid7 - миллисекунды Unix
	t := time.UnixMilli(int64(binary.BigEndian.Uint64(b[:]))).UTC()
	return path.Join(fmt.Sprintf("%04d", t.Year()), fmt.Sprintf("%02d", int(t.Month())))
}

// Put атомарно записывает картинку. Вызывается до записи метаданных: скан существует,
// только когда есть строка в БД, поэтому недописанный файл никому не виден.
func (s *Store) Put(id uuid.UUID, data []byte) (err error) {
	dst := s.Path(id)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.tmp, "*"+partSuffix)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Remove удаляет картинку - откат, если метаданные записать не удалось.
func (s *Store) Remove(id uuid.UUID) error {
	err := os.Remove(s.Path(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
