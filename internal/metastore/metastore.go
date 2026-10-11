// Package metastore хранит метаинформацию сканов (служебные поля + JSON клиента).
package metastore

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound = errors.New("скан не найден")
	// ErrInvalidMeta - БД отвергла данные (например, число вне диапазона numeric в jsonb).
	ErrInvalidMeta = errors.New("метаданные не принимаются базой данных")
	// ErrGroupNotFound - указанной группы (docgroup) нет: в существующую группу можно добавлять,
	// новую выдаёт только сервер.
	ErrGroupNotFound = errors.New("группа не найдена")
)

type Record struct {
	ID         uuid.UUID
	File       string  // путь к картинке относительно корня хранилища, разделитель "/"
	DocType    string  // "type" из JSON клиента
	DocNum     *string // номер документа без пробелов, тире и скобок
	PersonName *string
	PersonBday *time.Time      // только дата (00:00 UTC)
	MetaInfo   json.RawMessage // остальной JSON клиента (объект)
	// DocGroup объединяет сканы одной папки. При Insert 0 - выдать новую группу, иначе добавить в существующую;
	// после Insert содержит итоговое значение.
	DocGroup int64

	SHA256    string
	SizeBytes int64
	Width     int
	Height    int
	CreatedAt time.Time
}

// Store - всё, что API нужно от хранилища метаданных.
type Store interface {
	Insert(ctx context.Context, r *Record) error
	Get(ctx context.Context, id uuid.UUID) (*Record, error)
	// ListGroup возвращает все сканы группы в порядке загрузки (по id: uuid7 растёт со временем).
	// Для несуществующей группы список пуст.
	ListGroup(ctx context.Context, docgroup int64) ([]Record, error)
	Ping(ctx context.Context) error
}

// Memory - хранилище в памяти для тестов.
type Memory struct {
	mu        sync.Mutex
	recs      map[uuid.UUID]Record
	groups    map[int64]bool
	lastGroup int64
	// Fail, если задан, возвращается из Insert (проверка отката записи картинки).
	Fail error
}

func NewMemory() *Memory {
	return &Memory{recs: map[uuid.UUID]Record{}, groups: map[int64]bool{}}
}

func (m *Memory) Insert(_ context.Context, r *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	switch {
	case r.DocGroup == 0:
		m.lastGroup++
		r.DocGroup = m.lastGroup
		m.groups[r.DocGroup] = true
	case !m.groups[r.DocGroup]:
		return ErrGroupNotFound
	}
	m.recs[r.ID] = *r
	return nil
}

func (m *Memory) Get(_ context.Context, id uuid.UUID) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &r, nil
}

func (m *Memory) ListGroup(_ context.Context, docgroup int64) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.recs {
		if r.DocGroup == docgroup {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}

func (m *Memory) Ping(context.Context) error { return nil }
