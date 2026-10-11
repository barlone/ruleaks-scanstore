package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"scanstore-api-gate/internal/filestore"
	"scanstore-api-gate/internal/metastore"
)

// httpError - ошибка клиента с готовым кодом ответа.
type httpError struct {
	status int
	detail string
}

func (e *httpError) Error() string { return e.detail }

func clientErr(status int, format string, args ...any) error {
	return &httpError{status, fmt.Sprintf(format, args...)}
}

// multipartOverhead - запас на заголовки частей и границы сверх лимитов файла и meta.
const multipartOverhead = 64 * 1024

func (s *Server) addScan(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.opt.MaxUploadBytes+s.opt.MaxMetaBytes+multipartOverhead)
	up, err := s.readUpload(r)
	if err != nil {
		var he *httpError
		if errors.As(err, &he) {
			writeError(w, he.status, he.detail)
		} else {
			s.internalError(w, r, "приём файла", err)
		}
		return
	}

	id, err := uuid.NewV7()
	if err != nil {
		s.internalError(w, r, "генерация uuid", err)
		return
	}
	sum := sha256.Sum256(up.data)
	rec := &metastore.Record{
		ID:         id,
		File:       filestore.Rel(id),
		DocType:    up.doc.Type,
		DocNum:     up.doc.Num,
		PersonName: up.doc.Name,
		PersonBday: up.doc.Bday,
		MetaInfo:   up.doc.MetaInfo,
		DocGroup:   up.docGroup,
		SHA256:     hex.EncodeToString(sum[:]),
		SizeBytes:  int64(len(up.data)),
		Width:      up.width,
		Height:     up.height,
		CreatedAt:  time.Now().UTC().Truncate(time.Microsecond), // точность timestamptz
	}
	// Картинка пишется раньше метаданных: скан существует, только когда есть запись в БД.
	if err := s.files.Put(id, up.data); err != nil {
		s.internalError(w, r, "запись картинки", err)
		return
	}
	if err := s.meta.Insert(r.Context(), rec); err != nil {
		if rmErr := s.files.Remove(id); rmErr != nil {
			s.log.Error("откат: не удалось удалить картинку", "id", id, "err", rmErr)
		}
		if errors.Is(err, metastore.ErrInvalidMeta) {
			writeError(w, http.StatusUnprocessableEntity, "meta: "+err.Error())
			return
		}
		if errors.Is(err, metastore.ErrGroupNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "docgroup: группа не найдена (номер выдаёт сервер при загрузке первого скана папки)")
			return
		}
		s.internalError(w, r, "запись метаданных", err)
		return
	}

	w.Header().Set("Location", "/scans/"+id.String())
	writeJSON(w, http.StatusCreated, uploadJSON{
		ID: id.String(), DocGroup: rec.DocGroup, SHA256: rec.SHA256, SizeBytes: rec.SizeBytes,
		Width: rec.Width, Height: rec.Height, CreatedAt: formatTime(rec.CreatedAt),
	})
}

type upload struct {
	data          []byte
	width, height int
	doc           *docMeta
	docGroup      int64 // 0 - поле не передано: сервер выдаст новую группу
}

// readUpload читает multipart потоком (без временных файлов) и проверяет обе части.
func (s *Server) readUpload(r *http.Request) (*upload, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, clientErr(http.StatusUnsupportedMediaType, "ожидается multipart/form-data с полями file и meta")
	}
	var (
		up        upload
		haveFile  bool
		haveGroup bool
		rawMeta   []byte
	)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, bodyErr(err)
		}
		switch part.FormName() {
		case "file":
			if haveFile {
				return nil, clientErr(http.StatusUnprocessableEntity, "поле file передано больше одного раза")
			}
			haveFile = true
			if up.data, err = readLimited(part, s.opt.MaxUploadBytes, "файл больше %s", mb(s.opt.MaxUploadBytes)); err != nil {
				return nil, err
			}
		case "meta":
			if rawMeta != nil {
				return nil, clientErr(http.StatusUnprocessableEntity, "поле meta передано больше одного раза")
			}
			if rawMeta, err = readLimited(part, s.opt.MaxMetaBytes, "meta больше %d КБ", s.opt.MaxMetaBytes/1024); err != nil {
				return nil, err
			}
		case "docgroup":
			if haveGroup {
				return nil, clientErr(http.StatusUnprocessableEntity, "поле docgroup передано больше одного раза")
			}
			haveGroup = true
			raw, err := readLimited(part, 32, "docgroup должен быть целым числом")
			if err != nil {
				return nil, err
			}
			// пустое значение допустимо: как будто поля нет (скан получит новую группу)
			if txt := strings.TrimSpace(string(raw)); txt != "" {
				if up.docGroup, err = strconv.ParseInt(txt, 10, 64); err != nil || up.docGroup < 1 {
					return nil, clientErr(http.StatusUnprocessableEntity, "docgroup должен быть целым числом больше нуля")
				}
			}
		default:
			if _, err := io.Copy(io.Discard, part); err != nil {
				return nil, bodyErr(err)
			}
		}
	}
	if !haveFile {
		return nil, clientErr(http.StatusUnprocessableEntity, "нужен файл в поле file")
	}
	obj, err := parseMeta(rawMeta)
	if err != nil {
		return nil, err
	}
	if up.doc, err = splitMeta(obj); err != nil {
		return nil, err
	}
	if up.width, up.height, err = s.checkJPEG(up.data); err != nil {
		return nil, err
	}
	return &up, nil
}

func readLimited(part *multipart.Part, limit int64, tooBig string, args ...any) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return nil, bodyErr(err)
	}
	if int64(len(data)) > limit {
		return nil, clientErr(http.StatusRequestEntityTooLarge, tooBig, args...)
	}
	return data, nil
}

func bodyErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return clientErr(http.StatusRequestEntityTooLarge, "запрос больше %s", mb(mbe.Limit))
	}
	return clientErr(http.StatusBadRequest, "некорректное тело multipart: %v", err)
}

func mb(n int64) string { return fmt.Sprintf("%.4g МБ", float64(n)/(1024*1024)) }

// checkJPEG: файл должен полностью декодироваться (обрезанный не попадёт в хранилище).
// Хранится он как получен, без перекодирования.
func (s *Server) checkJPEG(data []byte) (width, height int, err error) {
	bad := clientErr(http.StatusUnprocessableEntity, "file должен быть корректным JPEG-изображением")
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, bad
	}
	if int64(cfg.Width)*int64(cfg.Height) > s.opt.MaxImagePixels {
		return 0, 0, clientErr(http.StatusUnprocessableEntity, "изображение %dx%d больше %d мегапикселей",
			cfg.Width, cfg.Height, s.opt.MaxImagePixels/1_000_000)
	}
	s.decodeSlots <- struct{}{}
	_, err = jpeg.Decode(bytes.NewReader(data))
	<-s.decodeSlots
	if err != nil {
		return 0, 0, bad
	}
	return cfg.Width, cfg.Height, nil
}

// parseMeta проверяет, что meta - JSON-объект, пригодный для jsonb: одиночные суррогаты и битый UTF-8
// при разборе заменяются, \u0000 отвергается (jsonb его не хранит). Числа остаются json.Number,
// поэтому большие значения не теряются при обратной записи.
func parseMeta(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, clientErr(http.StatusUnprocessableEntity, `нужно поле meta (JSON-объект с полем "type")`)
	}
	invalid := clientErr(http.StatusUnprocessableEntity, "meta должен быть корректным JSON")
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, invalid
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, invalid // после объекта что-то ещё
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, clientErr(http.StatusUnprocessableEntity, "meta должен быть JSON-объектом")
	}
	if hasNUL(v) {
		return nil, clientErr(http.StatusUnprocessableEntity, `meta не может содержать символ \u0000`)
	}
	return obj, nil
}

func hasNUL(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.IndexByte(t, 0) >= 0
	case map[string]any:
		for k, x := range t {
			if strings.IndexByte(k, 0) >= 0 || hasNUL(x) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if hasNUL(x) {
				return true
			}
		}
	}
	return false
}
