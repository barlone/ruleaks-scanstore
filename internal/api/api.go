// Package api - HTTP-обработчики хранилища сканов.
//
// Методы (кроме /health нужен заголовок "Authorization: Bearer <api_token>"):
//
//	POST /scans            multipart: file=<JPEG>, meta=<JSON-объект с полем "type">, docgroup=<номер, необязательно>
//	                       -> 201 {id, docgroup, ...}; без docgroup скан получает новую группу, а её номер из ответа
//	                       передают с остальными сканами той же папки
//	GET  /scans/{id}       картинка (поддерживаются Range, If-None-Match, If-Modified-Since)
//	GET  /groups/{n}       {docgroup, count, scans: [...]} - метаданные всех сканов группы в порядке загрузки;
//	                       поля metainfo и sha256 - только при ?metainfo=yes и ?sha256=yes
//	GET  /scans/{id}/meta  колонки таблицы scans; "metainfo" - переданный meta без вынесенных в колонки значений
//	GET  /health           проверка живости и доступности БД
//
// Ошибки - JSON {"detail": "..."}, как в прототипе на FastAPI.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"scanstore-api-gate/internal/filestore"
	"scanstore-api-gate/internal/metastore"
)

type Options struct {
	APIToken        string
	MaxUploadBytes  int64
	MaxMetaBytes    int64
	MaxImagePixels  int64
	MaxParallelJPEG int // 0 - по числу ядер
}

type Server struct {
	opt       Options
	tokenHash [sha256.Size]byte
	files     *filestore.Store
	meta      metastore.Store
	log       *slog.Logger
	// Полное декодирование JPEG съедает ширина*высота*3 байт памяти - ограничиваем параллельность.
	decodeSlots chan struct{}
}

func New(opt Options, files *filestore.Store, meta metastore.Store, log *slog.Logger) *Server {
	n := opt.MaxParallelJPEG
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	return &Server{
		opt:         opt,
		tokenHash:   sha256.Sum256([]byte(opt.APIToken)),
		files:       files,
		meta:        meta,
		log:         log,
		decodeSlots: make(chan struct{}, n),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /scans", s.auth(http.HandlerFunc(s.addScan)))
	mux.Handle("GET /scans/{id}", s.auth(http.HandlerFunc(s.getScan)))
	mux.Handle("GET /scans/{id}/meta", s.auth(http.HandlerFunc(s.getMeta)))
	mux.Handle("GET /groups/{docgroup}", s.auth(http.HandlerFunc(s.getGroup)))
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "Not Found")
	})
	return s.accessLog(mux)
}

// auth сравнивает хэши, а не сами токены: время сравнения не зависит и от длины.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		got := sha256.Sum256([]byte(strings.TrimSpace(token)))
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(got[:], s.tokenHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "неверный или отсутствующий токен")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Ответы - порядок и имена полей как в прототипе.
type uploadJSON struct {
	ID        string `json:"id"`
	DocGroup  int64  `json:"docgroup"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	CreatedAt string `json:"created_at"`
}

type recordJSON struct {
	ID         string  `json:"id"`
	File       string  `json:"file"`
	DocGroup   int64   `json:"docgroup"`
	DocType    string  `json:"doctype"`
	DocNum     *string `json:"docnum"`
	PersonName *string `json:"person_name"`
	PersonBday *string `json:"person_bday"` // ГГГГ-ММ-ДД
	SHA256     string  `json:"sha256"`
	SizeBytes  int64   `json:"size_bytes"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	CreatedAt  string  `json:"created_at"`
	// всегда задан (nil заменяется на null)
	MetaInfo json.RawMessage `json:"metainfo"`
}

// groupScanJSON - скан в выдаче группы: без file, docgroup (он на верхнем уровне) и created_at;
// sha256 и metainfo - только если их просили (?sha256=yes, ?metainfo=yes).
type groupScanJSON struct {
	ID         string          `json:"id"`
	DocType    string          `json:"doctype"`
	DocNum     *string         `json:"docnum"`
	PersonName *string         `json:"person_name"`
	PersonBday *string         `json:"person_bday"` // ГГГГ-ММ-ДД
	SizeBytes  int64           `json:"size_bytes"`
	Width      int             `json:"width"`
	Height     int             `json:"height"`
	SHA256     string          `json:"sha256,omitempty"`
	MetaInfo   json.RawMessage `json:"metainfo,omitempty"`
}

func newGroupScanJSON(rec *metastore.Record, withSHA, withMeta bool) groupScanJSON {
	r := newRecordJSON(rec)
	out := groupScanJSON{
		ID: r.ID, DocType: r.DocType, DocNum: r.DocNum, PersonName: r.PersonName, PersonBday: r.PersonBday,
		SizeBytes: r.SizeBytes, Width: r.Width, Height: r.Height,
	}
	if withSHA {
		out.SHA256 = r.SHA256
	}
	if withMeta {
		out.MetaInfo = r.MetaInfo
	}
	return out
}

// yesNoParam разбирает необязательный параметр запроса yes/no (регистр не важен): без параметра - false.
// При другом значении сам пишет ответ 422 и возвращает ok == false.
func yesNoParam(w http.ResponseWriter, r *http.Request, name string) (value, ok bool) {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "yes":
		return true, true
	case "", "no":
		return false, true
	}
	writeError(w, http.StatusUnprocessableEntity, name+" принимает значения yes или no")
	return false, false
}

// formatTime - как datetime.isoformat(timespec="seconds") в прототипе: 2026-10-08T12:00:00+00:00
func formatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05-07:00") }

func newRecordJSON(rec *metastore.Record) recordJSON {
	out := recordJSON{
		ID: rec.ID.String(), File: rec.File, DocGroup: rec.DocGroup, DocType: rec.DocType, DocNum: rec.DocNum, PersonName: rec.PersonName,
		SHA256: rec.SHA256, SizeBytes: rec.SizeBytes, Width: rec.Width, Height: rec.Height,
		CreatedAt: formatTime(rec.CreatedAt), MetaInfo: rec.MetaInfo,
	}
	if rec.PersonBday != nil {
		d := rec.PersonBday.Format("2006-01-02")
		out.PersonBday = &d
	}
	if out.MetaInfo == nil {
		out.MetaInfo = json.RawMessage("null") // колонка metainfo допускает NULL
	}
	return out
}

func (s *Server) getMeta(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newRecordJSON(rec))
}

type groupJSON struct {
	DocGroup int64           `json:"docgroup"`
	Count    int             `json:"count"`
	Scans    []groupScanJSON `json:"scans"`
}

// getGroup отдаёт метаданные всех сканов группы в порядке загрузки (uuid7 растёт со временем).
// metainfo (остаток JSON клиента, с OCR-текстом он самая тяжёлая часть) включается только при ?metainfo=yes,
// sha256 - только при ?sha256=yes.
// Картинки по-прежнему забираются по id через GET /scans/{id}.
func (s *Server) getGroup(w http.ResponseWriter, r *http.Request) {
	group, err := strconv.ParseInt(r.PathValue("docgroup"), 10, 64)
	if err != nil || group < 1 {
		writeError(w, http.StatusUnprocessableEntity, "docgroup должен быть целым числом больше нуля")
		return
	}
	withMeta, ok := yesNoParam(w, r, "metainfo")
	if !ok {
		return
	}
	withSHA, ok := yesNoParam(w, r, "sha256")
	if !ok {
		return
	}
	recs, err := s.meta.ListGroup(r.Context(), group)
	if err != nil {
		s.internalError(w, r, "чтение группы", err)
		return
	}
	if len(recs) == 0 {
		writeError(w, http.StatusNotFound, "группа не найдена")
		return
	}
	out := groupJSON{DocGroup: group, Count: len(recs), Scans: make([]groupScanJSON, len(recs))}
	for i := range recs {
		out.Scans[i] = newGroupScanJSON(&recs[i], withSHA, withMeta)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getScan(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}
	path, err := s.files.Abs(rec.File)
	if err != nil {
		s.log.Error("путь к файлу скана в БД некорректен", "id", rec.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "файл скана недоступен")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		// запись в БД есть, а файла нет - потеря данных, а не "не найдено"
		s.log.Error("файл скана недоступен", "id", rec.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "файл скана недоступен")
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("ETag", `"`+rec.SHA256+`"`)
	// файл неизменяем, поэтому кэшировать можно надолго (но не в общих кэшах: это персональные данные)
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, "", rec.CreatedAt, f)
}

// lookup разбирает {id} и читает запись; при ошибке сам пишет ответ.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*metastore.Record, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "id должен быть UUID")
		return nil, false
	}
	rec, err := s.meta.Get(r.Context(), id)
	switch {
	case errors.Is(err, metastore.ErrNotFound):
		writeError(w, http.StatusNotFound, "скан не найден")
		return nil, false
	case err != nil:
		s.internalError(w, r, "чтение метаданных", err)
		return nil, false
	}
	return rec, true
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.meta.Ping(ctx); err != nil {
		s.log.Warn("health: БД недоступна", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return // клиент ушёл, отвечать некому
	}
	s.log.Error(what, "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
