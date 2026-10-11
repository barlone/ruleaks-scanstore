package metastore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Интеграционный тест: нужна пустая (или тестовая) база, например
//
//	$env:SCANSTORE_TEST_DSN = "postgres://scanstore:secret@localhost:5432/scanstore_test?sslmode=disable"
//	go test ./internal/metastore
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("SCANSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("SCANSTORE_TEST_DSN не задан")
	}
	ctx := context.Background()
	db, err := OpenPostgres(ctx, dsn, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	// t.Cleanup идут в обратном порядке: Close (зарегистрирован первым) выполнится после удаления строки.
	// С defer пул закрывался бы раньше, и тестовая строка оставалась в базе.
	t.Cleanup(db.Close)
	// повторная миграция ничего не ломает
	db2, err := OpenPostgres(ctx, dsn, 1, true)
	if err != nil {
		t.Fatal("повторная миграция:", err)
	}
	db2.Close()

	id, _ := uuid.NewV7()
	docnum, person := "9218506640", "ЕГОРОВ СЕРГЕЙ"
	bday := time.Date(1973, 11, 9, 0, 0, 0, 0, time.UTC)
	rec := &Record{
		ID: id, File: "2026/10/" + id.String() + ".jpg", DocType: "passport",
		DocNum: &docnum, PersonName: &person, PersonBday: &bday,
		MetaInfo: json.RawMessage(`{"n":12345678901234567890,"ФИО":"Егоров"}`),
		SHA256:   "ab", SizeBytes: 10, Width: 3, Height: 4,
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	ids := []uuid.UUID{id} // всё, что тест вставил, удаляется в конце
	t.Cleanup(func() {
		if _, err := db.pool.Exec(ctx, "DELETE FROM scans WHERE id = ANY($1)", ids); err != nil {
			t.Errorf("не удалось удалить тестовые строки %v: %v", ids, err)
		}
	})
	if err := db.Insert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if rec.DocGroup < 1 {
		t.Fatalf("первая вставка без группы должна выдать новую, получено %d", rec.DocGroup)
	}

	got, err := db.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.DocGroup != rec.DocGroup {
		t.Fatalf("docgroup из БД %d, при вставке %d", got.DocGroup, rec.DocGroup)
	}

	// группы: добавление в существующую, новая группа, несуществующая группа
	same, _ := uuid.NewV7()
	r2 := *rec
	r2.ID, r2.File = same, "2026/10/"+same.String()+".jpg"
	ids = append(ids, same)
	if err := db.Insert(ctx, &r2); err != nil || r2.DocGroup != rec.DocGroup {
		t.Fatalf("вставка в существующую группу %d: группа %d, ошибка %v", rec.DocGroup, r2.DocGroup, err)
	}
	other, _ := uuid.NewV7()
	r3 := *rec
	r3.ID, r3.File, r3.DocGroup = other, "2026/10/"+other.String()+".jpg", 0
	ids = append(ids, other)
	if err := db.Insert(ctx, &r3); err != nil || r3.DocGroup == rec.DocGroup || r3.DocGroup < 1 {
		t.Fatalf("новая группа: %d (первая %d), ошибка %v", r3.DocGroup, rec.DocGroup, err)
	}
	// ListGroup: вся группа в порядке id, другие группы не примешиваются, у несуществующей список пуст
	list, err := db.ListGroup(ctx, rec.DocGroup)
	if err != nil || len(list) != 2 || list[0].ID != id || list[1].ID != same ||
		list[0].DocGroup != rec.DocGroup || list[0].DocType != "passport" || *list[0].DocNum != docnum {
		t.Fatalf("ListGroup(%d): %v, %+v", rec.DocGroup, err, list)
	}
	if list, err = db.ListGroup(ctx, r3.DocGroup); err != nil || len(list) != 1 || list[0].ID != other {
		t.Fatalf("ListGroup(%d): %v, %+v", r3.DocGroup, err, list)
	}
	if list, err = db.ListGroup(ctx, r3.DocGroup+1000); err != nil || len(list) != 0 {
		t.Fatalf("ListGroup несуществующей группы: %v, %+v", err, list)
	}

	ghost, _ := uuid.NewV7()
	r4 := *rec
	r4.ID, r4.File, r4.DocGroup = ghost, "2026/10/"+ghost.String()+".jpg", r3.DocGroup+1000
	if err := db.Insert(ctx, &r4); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("несуществующая группа: ожидалась ErrGroupNotFound, получено %v", err)
	}
	if _, err := db.Get(ctx, ghost); !errors.Is(err, ErrNotFound) {
		t.Fatalf("после отказа строка не должна появиться: %v", err)
	}
	if !got.CreatedAt.Equal(rec.CreatedAt) || got.SHA256 != "ab" || got.SizeBytes != 10 || got.Width != 3 || got.Height != 4 {
		t.Fatalf("запись искажена: %+v", got)
	}
	if got.File != rec.File || got.DocType != "passport" || *got.DocNum != docnum || *got.PersonName != person ||
		got.PersonBday == nil || !got.PersonBday.Equal(bday) {
		t.Fatalf("колонки документа искажены: %+v", got)
	}
	var meta struct {
		N   json.Number `json:"n"`
		FIO string      `json:"ФИО"`
	}
	if err := json.Unmarshal(got.MetaInfo, &meta); err != nil || meta.N != "12345678901234567890" || meta.FIO != "Егоров" {
		t.Fatalf("metainfo искажена: %s (%v)", got.MetaInfo, err)
	}

	if err := db.Insert(ctx, rec); err == nil || errors.Is(err, ErrInvalidMeta) {
		t.Fatalf("повторный id должен давать ошибку БД, а не ErrInvalidMeta: %v", err)
	}

	id2, _ := uuid.NewV7()
	bad := *rec
	bad.ID = id2
	bad.File = "2026/10/" + id2.String() + ".jpg" // иначе сработает unique(file), а не проверка jsonb
	bad.MetaInfo = json.RawMessage(`{"a":"x\u0000y"}`)
	if err := db.Insert(ctx, &bad); !errors.Is(err, ErrInvalidMeta) {
		t.Fatalf("\\u0000 в jsonb: ожидалась ErrInvalidMeta, получено %v", err)
	}

	if _, err := db.Get(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ожидалась ErrNotFound, получено %v", err)
	}
}
