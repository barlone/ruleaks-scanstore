package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"scanstore-api-gate/internal/filestore"
	"scanstore-api-gate/internal/metastore"
)

const token = "test-token-0123456789"

type env struct {
	t    *testing.T
	h    http.Handler
	meta *metastore.Memory
	root string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	files, err := filestore.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	meta := metastore.NewMemory()
	srv := New(Options{
		APIToken: token, MaxUploadBytes: 1 << 20, MaxMetaBytes: 1024, MaxImagePixels: 1_000_000,
	}, files, meta, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &env{t: t, h: srv.Handler(), meta: meta, root: root}
}

func (e *env) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) get(path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return e.do(req)
}

type part struct{ name, filename, content string }

func uploadReq(parts ...part) *http.Request {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, p := range parts {
		var w io.Writer
		if p.filename != "" {
			w, _ = mw.CreateFormFile(p.name, p.filename)
		} else {
			w, _ = mw.CreateFormField(p.name)
		}
		_, _ = io.WriteString(w, p.content)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/scans", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{200, 30, 30, 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("ответ не JSON: %v: %s", err, rec.Body)
	}
	return v
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("код %d, ожидался %d: %s", rec.Code, status, rec.Body)
	}
}

// okMeta - минимальный допустимый meta.
const okMeta = `{"type":"passport"}`

func TestUploadAndRead(t *testing.T) {
	e := newEnv(t)
	img := makeJPEG(t, 120, 80)
	meta := `{"type":"passport","folder_person":"Егоров Сергей <А>","n":12345678901234567890,` +
		`"fields":{"series":"9218","number":"506640","surname":"ЕГОРОВ","name":"СЕРГЕЙ","patronymic":"АЛЕКСАНДРОВИЧ",` +
		`"birth_date":"09.11.1973","issued_by":"МВД","x":[1,2.50]}}`
	rec := e.do(uploadReq(part{"meta", "", meta}, part{"file", "scan107_p2__passport.jpg", string(img)}))
	expect(t, rec, http.StatusCreated)

	up := decode[map[string]any](t, rec)
	id := up["id"].(string)
	sum := sha256.Sum256(img)
	if up["sha256"] != hex.EncodeToString(sum[:]) || up["width"] != 120.0 || up["height"] != 80.0 ||
		up["size_bytes"] != float64(len(img)) {
		t.Fatalf("неверный ответ: %v", up)
	}
	if rec.Header().Get("Location") != "/scans/"+id {
		t.Fatalf("Location = %q", rec.Header().Get("Location"))
	}
	if !strings.HasSuffix(up["created_at"].(string), "+00:00") {
		t.Fatalf("created_at = %v", up["created_at"])
	}

	// метаданные: колонки + остальной JSON клиента без потерь (в т.ч. большое число и кириллица)
	rec = e.get("/scans/" + id + "/meta")
	expect(t, rec, http.StatusOK)
	got := decode[struct {
		ID         string          `json:"id"`
		File       string          `json:"file"`
		DocType    string          `json:"doctype"`
		DocNum     *string         `json:"docnum"`
		PersonName *string         `json:"person_name"`
		PersonBday *string         `json:"person_bday"`
		MetaInfo   json.RawMessage `json:"metainfo"`
	}](t, rec)
	if got.ID != id || bytes.Contains(rec.Body.Bytes(), []byte("original_name")) {
		t.Fatalf("неверные метаданные: %s", rec.Body)
	}
	if got.DocType != "passport" || got.DocNum == nil || *got.DocNum != "9218506640" ||
		got.PersonName == nil || *got.PersonName != "ЕГОРОВ СЕРГЕЙ <А>" || // из folder_person, в верхнем регистре
		got.PersonBday == nil || *got.PersonBday != "1973-11-09" {
		t.Fatalf("неверные колонки: %s", rec.Body)
	}
	if !bytes.Contains(got.MetaInfo, []byte(`12345678901234567890`)) || !bytes.Contains(got.MetaInfo, []byte(`"Егоров Сергей <А>"`)) {
		t.Fatalf("metainfo искажена: %s", got.MetaInfo)
	}
	// вынесенное в колонки из metainfo убрано, остальное на месте
	for _, gone := range []string{`"type"`, `"series"`, `"number"`, `"birth_date"`} {
		if bytes.Contains(got.MetaInfo, []byte(gone)) {
			t.Fatalf("в metainfo остался %s: %s", gone, got.MetaInfo)
		}
	}
	if !bytes.Contains(got.MetaInfo, []byte(`"issued_by":"МВД"`)) {
		t.Fatalf("в metainfo потеряно issued_by: %s", got.MetaInfo)
	}

	// картинка отдаётся байт в байт
	rec = e.get("/scans/" + id)
	expect(t, rec, http.StatusOK)
	if !bytes.Equal(rec.Body.Bytes(), img) || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("картинка искажена (%d байт, %s)", rec.Body.Len(), rec.Header().Get("Content-Type"))
	}
	etag := rec.Header().Get("ETag")
	if etag != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Fatalf("ETag = %q", etag)
	}
	expect(t, e.get("/scans/"+id, "If-None-Match", etag), http.StatusNotModified)

	rec = e.get("/scans/"+id, "Range", "bytes=0-9")
	expect(t, rec, http.StatusPartialContent)
	if !bytes.Equal(rec.Body.Bytes(), img[:10]) {
		t.Fatal("неверный диапазон")
	}

	// файл лежит в store/ГГГГ/ММ/<uuid>.jpg, и scans.file указывает именно на него
	matches, _ := filepath.Glob(filepath.Join(e.root, "*", "*", id+".jpg"))
	if len(matches) != 1 {
		t.Fatalf("файл не найден в раскладке ГГГГ/ММ: %v", matches)
	}
	if want := filepath.ToSlash(strings.TrimPrefix(matches[0], e.root+string(filepath.Separator))); got.File != want {
		t.Fatalf("file = %q, ожидалось %q", got.File, want)
	}
}

// upload загружает скан (с docgroup, если он не пустой) и возвращает id и docgroup из ответа.
func (e *env) upload(group string) (id string, docgroup int64) {
	e.t.Helper()
	parts := []part{{"meta", "", okMeta}}
	if group != "" {
		parts = append(parts, part{"docgroup", "", group})
	}
	parts = append(parts, part{"file", "a.jpg", string(makeJPEG(e.t, 10, 10))})
	rec := e.do(uploadReq(parts...))
	expect(e.t, rec, http.StatusCreated)
	up := decode[struct {
		ID       string `json:"id"`
		DocGroup int64  `json:"docgroup"`
	}](e.t, rec)
	return up.ID, up.DocGroup
}

func TestDocGroup(t *testing.T) {
	e := newEnv(t)

	// без docgroup скан получает новую группу
	idA, g1 := e.upload("")
	if g1 < 1 {
		t.Fatalf("docgroup = %d", g1)
	}
	// остальные сканы папки передают этот номер и остаются в той же группе
	idB, gB := e.upload(strconv.FormatInt(g1, 10))
	if gB != g1 {
		t.Fatalf("docgroup второго скана %d, ожидался %d", gB, g1)
	}
	// пустое поле - как отсутствующее: новая группа
	_, g2 := e.upload(" ")
	if g2 == g1 || g2 < 1 {
		t.Fatalf("пустой docgroup дал группу %d (первая %d)", g2, g1)
	}
	// в метаданных группа та же, что вернула загрузка
	for id, want := range map[string]int64{idA: g1, idB: g1} {
		got := decode[struct {
			DocGroup int64 `json:"docgroup"`
		}](t, e.get("/scans/"+id+"/meta"))
		if got.DocGroup != want {
			t.Fatalf("/meta %s: docgroup %d, ожидался %d", id, got.DocGroup, want)
		}
	}

	// несуществующая группа отклоняется, картинка не остаётся
	before := countJPEG(t, e.root)
	rec := e.do(uploadReq(part{"meta", "", okMeta}, part{"docgroup", "", strconv.FormatInt(g2+100, 10)},
		part{"file", "a.jpg", string(makeJPEG(t, 10, 10))}))
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(decode[map[string]string](t, rec)["detail"], "docgroup") {
		t.Fatalf("в detail нет docgroup: %s", rec.Body)
	}
	if after := countJPEG(t, e.root); after != before {
		t.Fatalf("после отказа в хранилище %d картинок, было %d", after, before)
	}
}

func TestGetGroup(t *testing.T) {
	e := newEnv(t)
	meta := `{"type":"snils","folder_person":"Егоров Сергей","fields":{"snils":"072-261-638 47"}}`
	up := func(group string, extra ...part) string {
		parts := []part{{"meta", "", meta}, {"file", "a.jpg", string(makeJPEG(t, 10, 10))}}
		if group != "" {
			parts = append(parts, part{"docgroup", "", group})
		}
		rec := e.do(uploadReq(append(parts, extra...)...))
		expect(t, rec, http.StatusCreated)
		return decode[struct {
			ID       string `json:"id"`
			DocGroup int64  `json:"docgroup"`
		}](t, rec).ID
	}
	idA := up("")
	g := decode[struct {
		DocGroup int64 `json:"docgroup"`
	}](t, e.get("/scans/"+idA+"/meta")).DocGroup
	gs := strconv.FormatInt(g, 10)
	idB := up(gs)
	idC := up(gs)
	idOther := up("") // отдельная группа

	rec := e.get("/groups/" + gs)
	expect(t, rec, http.StatusOK)
	got := decode[struct {
		DocGroup int64 `json:"docgroup"`
		Count    int   `json:"count"`
		Scans    []struct {
			ID         string          `json:"id"`
			DocType    string          `json:"doctype"`
			DocNum     *string         `json:"docnum"`
			PersonName *string         `json:"person_name"`
			SizeBytes  int64           `json:"size_bytes"`
			Width      int             `json:"width"`
			MetaInfo   json.RawMessage `json:"metainfo"`
		} `json:"scans"`
	}](t, rec)
	// в элементах нет file, docgroup, sha256 и created_at (docgroup есть только на верхнем уровне)
	var raw struct {
		Scans []map[string]json.RawMessage `json:"scans"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, sc := range raw.Scans {
		for _, k := range []string{"file", "docgroup", "sha256", "created_at", "metainfo"} {
			if _, has := sc[k]; has {
				t.Fatalf("в элементе группы есть ключ %q: %v", k, sc)
			}
		}
		for _, k := range []string{"id", "doctype", "docnum", "person_name", "person_bday", "size_bytes", "width", "height"} {
			if _, has := sc[k]; !has {
				t.Fatalf("в элементе группы нет ключа %q: %v", k, sc)
			}
		}
	}
	if got.DocGroup != g || got.Count != 3 || len(got.Scans) != 3 {
		t.Fatalf("группа %d: %s", g, rec.Body)
	}
	// порядок загрузки, и только своя группа
	for i, want := range []string{idA, idB, idC} {
		s := got.Scans[i]
		if s.ID != want || s.DocType != "snils" || s.SizeBytes == 0 || s.Width != 10 || len(s.MetaInfo) != 0 ||
			s.DocNum == nil || *s.DocNum != "07226163847" || s.PersonName == nil || *s.PersonName != "ЕГОРОВ СЕРГЕЙ" {
			t.Fatalf("скан %d группы: %+v", i, s)
		}
	}
	for _, s := range got.Scans {
		if s.ID == idOther {
			t.Fatal("в группе скан другой группы")
		}
	}

	// metainfo: по умолчанию и при metainfo=no его нет в ответе вообще; при metainfo=yes есть у каждого скана
	for _, q := range []string{"", "?metainfo=no", "?metainfo=NO", "?metainfo="} {
		if body := e.get("/groups/" + gs + q).Body.String(); strings.Contains(body, `"metainfo"`) {
			t.Fatalf("metainfo%q: в ответе есть metainfo: %s", q, body)
		}
	}
	for _, q := range []string{"?metainfo=yes", "?metainfo=YES"} {
		rec := e.get("/groups/" + gs + q)
		expect(t, rec, http.StatusOK)
		with := decode[struct {
			Scans []struct {
				MetaInfo json.RawMessage `json:"metainfo"`
			} `json:"scans"`
		}](t, rec)
		if len(with.Scans) != 3 {
			t.Fatalf("%s: %d сканов", q, len(with.Scans))
		}
		for _, s := range with.Scans {
			if !bytes.Contains(s.MetaInfo, []byte(`"folder_person":"Егоров Сергей"`)) {
				t.Fatalf("%s: metainfo = %s", q, s.MetaInfo)
			}
		}
	}
	for _, q := range []string{"?metainfo=true", "?metainfo=1", "?metainfo=yes,no", "?sha256=true", "?sha256=1", "?metainfo=yes&sha256=maybe"} {
		expect(t, e.get("/groups/"+gs+q), http.StatusUnprocessableEntity)
	}

	// sha256: по умолчанию нет, при sha256=yes равен хешу из /meta, не зависит от metainfo
	for _, q := range []string{"", "?sha256=no", "?sha256=", "?metainfo=yes"} {
		if body := e.get("/groups/" + gs + q).Body.String(); strings.Contains(body, `"sha256"`) {
			t.Fatalf("sha256%q: в ответе есть sha256: %s", q, body)
		}
	}
	for _, q := range []string{"?sha256=yes", "?sha256=YES", "?sha256=yes&metainfo=no", "?metainfo=yes&sha256=yes"} {
		rec := e.get("/groups/" + gs + q)
		expect(t, rec, http.StatusOK)
		withSHA := decode[struct {
			Scans []struct {
				ID       string          `json:"id"`
				SHA256   string          `json:"sha256"`
				MetaInfo json.RawMessage `json:"metainfo"`
			} `json:"scans"`
		}](t, rec)
		if len(withSHA.Scans) != 3 {
			t.Fatalf("%s: %d сканов", q, len(withSHA.Scans))
		}
		for _, s := range withSHA.Scans {
			want := decode[struct {
				SHA256 string `json:"sha256"`
			}](t, e.get("/scans/"+s.ID+"/meta")).SHA256
			if len(want) != 64 || s.SHA256 != want {
				t.Fatalf("%s: sha256 = %q, ожидался %q", q, s.SHA256, want)
			}
			if hasMeta := len(s.MetaInfo) > 0; hasMeta != strings.Contains(q, "metainfo=yes") {
				t.Fatalf("%s: metainfo присутствует = %v", q, hasMeta)
			}
		}
	}
	// /meta отдаёт metainfo всегда
	if body := e.get("/scans/" + idA + "/meta").Body.String(); !strings.Contains(body, `"metainfo"`) {
		t.Fatalf("/meta без metainfo: %s", body)
	}

	// группа из одного скана, у других сканов своя
	other := decode[struct {
		DocGroup int64 `json:"docgroup"`
	}](t, e.get("/scans/"+idOther+"/meta")).DocGroup
	one := decode[struct{ Count int }](t, e.get("/groups/"+strconv.FormatInt(other, 10)))
	if one.Count != 1 {
		t.Fatalf("count = %d, ожидался 1", one.Count)
	}

	expect(t, e.get("/groups/"+strconv.FormatInt(g+1000, 10)), http.StatusNotFound)
	for _, bad := range []string{"abc", "0", "-3", "1.5", "99999999999999999999"} {
		expect(t, e.get("/groups/"+bad), http.StatusUnprocessableEntity)
	}

	// без токена
	req := httptest.NewRequest(http.MethodGet, "/groups/"+gs, nil)
	expect(t, e.do(req), http.StatusUnauthorized)
}

func countJPEG(t *testing.T, root string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jpg") {
			n++
		}
		return nil
	})
	return n
}

func TestUploadMinimalMeta(t *testing.T) {
	e := newEnv(t)
	rec := e.do(uploadReq(part{"meta", "", okMeta}, part{"file", "a.jpg", string(makeJPEG(t, 10, 10))}))
	expect(t, rec, http.StatusCreated)
	id := decode[map[string]any](t, rec)["id"].(string)
	m := decode[map[string]json.RawMessage](t, e.get("/scans/"+id+"/meta"))
	if string(m["doctype"]) != `"passport"` || string(m["docnum"]) != "null" || string(m["person_name"]) != "null" ||
		string(m["person_bday"]) != "null" || string(m["metainfo"]) != "{}" {
		t.Fatalf("неверные колонки: %v", m)
	}
}

func TestUploadRejected(t *testing.T) {
	e := newEnv(t)
	img := string(makeJPEG(t, 50, 50))
	cases := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"нет файла", uploadReq(part{"meta", "", okMeta}), 422},
		{"не JPEG", uploadReq(part{"meta", "", okMeta}, part{"file", "a.png", "\x89PNG\r\n\x1a\nxxxx"}), 422},
		{"обрезанный JPEG", uploadReq(part{"meta", "", okMeta}, part{"file", "a.jpg", img[:len(img)-40]}), 422},
		{"JPEG без EOI", uploadReq(part{"meta", "", okMeta}, part{"file", "a.jpg", img[:len(img)-2]}), 422},
		{"слишком много пикселей", uploadReq(part{"meta", "", okMeta}, part{"file", "a.jpg", string(makeJPEG(t, 1100, 1000))}), 422},
		{"нет meta", uploadReq(part{"file", "a.jpg", img}), 422},
		{"пустой meta", uploadReq(part{"file", "a.jpg", img}, part{"meta", "", ` `}), 422},
		{"meta без type", uploadReq(part{"file", "a.jpg", img}, part{"meta", "", `{"fields":{}}`}), 422},
		{"type не строка", uploadReq(part{"file", "a.jpg", img}, part{"meta", "", `{"type":5}`}), 422},
		{"type пустой", uploadReq(part{"file", "a.jpg", img}, part{"meta", "", `{"type":" "}`}), 422},
		{"meta не JSON", uploadReq(part{"file", "a.jpg", img}, part{"meta", "", `{"a":`}), 422},
		{"meta не объект", uploadReq(part{"file", "a.jpg", img}, part{"meta", "", `[1,2]`}), 422},
		{"meta с хвостом", uploadReq(part{"file", "a.jpg", img}, part{"meta", "", `{"type":"a"} {}`}), 422},
		{"meta с NUL", uploadReq(part{"file", "a.jpg", img}, part{"meta", "", `{"type":"a","a":"x\u0000y"}`}), 422},
		{"meta слишком большая", uploadReq(part{"file", "a.jpg", img}, part{"meta", "", `{"type":"a","a":"` + strings.Repeat("x", 2000) + `"}`}), 413},
		{"файл слишком большой", uploadReq(part{"meta", "", okMeta}, part{"file", "a.jpg", strings.Repeat("x", 1<<20+1)}), 413},
		{"два файла", uploadReq(part{"meta", "", okMeta}, part{"file", "a.jpg", img}, part{"file", "b.jpg", img}), 422},
		{"два meta", uploadReq(part{"meta", "", okMeta}, part{"meta", "", okMeta}, part{"file", "a.jpg", img}), 422},
		{"docgroup не число", uploadReq(part{"meta", "", okMeta}, part{"docgroup", "", "abc"}, part{"file", "a.jpg", img}), 422},
		{"docgroup дробный", uploadReq(part{"meta", "", okMeta}, part{"docgroup", "", "1.5"}, part{"file", "a.jpg", img}), 422},
		{"docgroup ноль", uploadReq(part{"meta", "", okMeta}, part{"docgroup", "", "0"}, part{"file", "a.jpg", img}), 422},
		{"docgroup отрицательный", uploadReq(part{"meta", "", okMeta}, part{"docgroup", "", "-5"}, part{"file", "a.jpg", img}), 422},
		{"docgroup слишком длинный", uploadReq(part{"meta", "", okMeta}, part{"docgroup", "", strings.Repeat("9", 40)}, part{"file", "a.jpg", img}), 413},
		{"docgroup не существует", uploadReq(part{"meta", "", okMeta}, part{"docgroup", "", "999"}, part{"file", "a.jpg", img}), 422},
		{"два docgroup", uploadReq(part{"meta", "", okMeta}, part{"docgroup", "", "1"}, part{"docgroup", "", "1"}, part{"file", "a.jpg", img}), 422},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.do(c.req)
			expect(t, rec, c.status)
			if decode[map[string]string](t, rec)["detail"] == "" {
				t.Fatal("нет detail")
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/scans", strings.NewReader(img))
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Authorization", "Bearer "+token)
	expect(t, e.do(req), http.StatusUnsupportedMediaType)

	// ни один отклонённый запрос не оставил файлов
	assertNoImages(t, e.root)
}

func TestInsertFailureRemovesImage(t *testing.T) {
	e := newEnv(t)
	e.meta.Fail = errors.New("БД упала")
	expect(t, e.do(uploadReq(part{"meta", "", okMeta}, part{"file", "a.jpg", string(makeJPEG(t, 10, 10))})), http.StatusInternalServerError)
	assertNoImages(t, e.root)

	e.meta.Fail = metastore.ErrInvalidMeta
	expect(t, e.do(uploadReq(part{"meta", "", okMeta}, part{"file", "a.jpg", string(makeJPEG(t, 10, 10))})), http.StatusUnprocessableEntity)
	assertNoImages(t, e.root)
}

func assertNoImages(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("лишний файл в хранилище: %s", path)
		}
		return nil
	})
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	for _, auth := range []string{"", "Bearer", "Bearer wrong-token-0123456789", "Basic " + token, token} {
		req := httptest.NewRequest(http.MethodGet, "/scans/0199c1a2-0000-7000-8000-000000000000/meta", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := e.do(req)
		expect(t, rec, http.StatusUnauthorized)
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("нет WWW-Authenticate")
		}
	}
	// схема без учёта регистра
	req := httptest.NewRequest(http.MethodGet, "/scans/0199c1a2-0000-7000-8000-000000000000/meta", nil)
	req.Header.Set("Authorization", "bearer "+token)
	expect(t, e.do(req), http.StatusNotFound)

	// /health без авторизации
	rec := e.do(httptest.NewRequest(http.MethodGet, "/health", nil))
	expect(t, rec, http.StatusOK)
	if decode[map[string]string](t, rec)["status"] != "ok" {
		t.Fatal(rec.Body)
	}
}

func TestNotFound(t *testing.T) {
	e := newEnv(t)
	expect(t, e.get("/scans/0199c1a2-0000-7000-8000-000000000000"), http.StatusNotFound)
	expect(t, e.get("/scans/0199c1a2-0000-7000-8000-000000000000/meta"), http.StatusNotFound)
	expect(t, e.get("/scans/not-a-uuid"), http.StatusUnprocessableEntity)
	expect(t, e.get("/nothing"), http.StatusNotFound)
}
