// Команда bulkload загружает сканы через API: обходит папку рекурсивно и для каждого
// <имя>.jpg отправляет POST /scans вместе с <имя>.json (поле meta).
//
//	go run ./cmd/bulkload -token $env:SCANSTORE_TOKEN
//	go run ./cmd/bulkload -dry-run                      # только посчитать пары, ничего не отправляя
//	go run ./cmd/bulkload -limit 20 -workers 2          # пробная загрузка
//
// Файлы одной папки получают общий docgroup: первый файл папки отправляется без него, сервер выдаёт
// новый номер группы, остальные файлы папки отправляются с этим номером. Папку обрабатывает один поток,
// параллельно идут разные папки.
//
// Каждый успешно загруженный файл дописывается в журнал (-out, по умолчанию bulkload.jsonl) вместе с id
// и docgroup. Повторный запуск пропускает файлы из журнала и достраивает группы уже начатых папок,
// поэтому прерванную загрузку можно продолжить.
// Сервер дубли не отсекает: без журнала повторная загрузка создаст вторую копию каждого скана.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	slashpath "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	dir     string
	url     string
	token   string
	out     string
	workers int
	limit   int
	dirs    int
	dryRun  bool
}

// pair - картинка и JSON с её описанием.
type pair struct {
	img, meta string
}

type summary struct {
	pairs      int            // найдено пар jpg+json
	skipped    int            // уже есть в журнале
	ok         int            // загружено
	failed     int            // не загружено
	byStatus   map[string]int // причины отказов: HTTP-код или "сеть"
	bytes      int64          // отправлено байт картинок
	noJSON     []string       // jpg без json
	noImage    []string       // json без jpg
	elapsed    time.Duration
	failedList []string
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.dir, "dir", `D:\PyProjs\DocScanSort\Output`, "папка со сканами (обход рекурсивный)")
	flag.StringVar(&cfg.url, "url", "http://127.0.0.1:8000", "адрес сервера")
	flag.StringVar(&cfg.token, "token", os.Getenv("SCANSTORE_TOKEN"), "api_token (или переменная SCANSTORE_TOKEN)")
	flag.StringVar(&cfg.out, "out", "bulkload.jsonl", "журнал загруженных файлов (для продолжения)")
	flag.IntVar(&cfg.workers, "workers", 4, "параллельных загрузок")
	flag.IntVar(&cfg.limit, "limit", 0, "загрузить не больше N файлов (0 - все)")
	flag.IntVar(&cfg.dirs, "dirs", 0, "загрузить не больше N папок целиком (0 - все); считается после -limit")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "только найти пары, ничего не отправлять")
	flag.Parse()

	if !cfg.dryRun && cfg.token == "" {
		fmt.Fprintln(os.Stderr, "нужен токен: -token или переменная SCANSTORE_TOKEN")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	s, err := run(ctx, cfg, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
	if s.failed > 0 || ctx.Err() != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, stdout, stderr io.Writer) (*summary, error) {
	if cfg.workers < 1 {
		cfg.workers = 1
	}
	s := &summary{byStatus: map[string]int{}}
	pairs, err := findPairs(cfg.dir, s)
	if err != nil {
		return nil, err
	}
	s.pairs = len(pairs)
	fmt.Fprintf(stdout, "найдено пар jpg+json: %d; jpg без json: %d; json без jpg: %d\n",
		len(pairs), len(s.noJSON), len(s.noImage))
	if cfg.dryRun {
		return s, nil
	}

	done, err := readLog(cfg.out)
	if err != nil {
		return nil, err
	}
	var todo []pair
	// группа каталога из журнала: продолжая прерванную загрузку, остальные файлы папки попадают в ту же группу
	groups := map[string]int64{}
	for img, rec := range done {
		if rec.DocGroup > 0 {
			groups[slashpath.Dir(img)] = rec.DocGroup
		}
	}
	for _, p := range pairs {
		if _, ok := done[rel(cfg.dir, p.img)]; ok {
			s.skipped++
			continue
		}
		todo = append(todo, p)
	}
	if cfg.limit > 0 && len(todo) > cfg.limit {
		todo = todo[:cfg.limit]
	}
	if s.skipped > 0 {
		fmt.Fprintf(stdout, "уже загружено (по журналу %s): %d\n", cfg.out, s.skipped)
	}
	dirs := groupByDir(cfg.dir, todo, groups)
	if cfg.dirs > 0 && len(dirs) > cfg.dirs {
		dirs = dirs[:cfg.dirs]
		todo = todo[:0:0]
		for _, d := range dirs {
			todo = append(todo, d.pairs...)
		}
	}

	logf, err := os.OpenFile(cfg.out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer logf.Close()

	var (
		mu      sync.Mutex // журнал и сводка
		counter atomic.Int64
		start   = time.Now()
		client  = &http.Client{Timeout: 5 * time.Minute}
		jobs    = make(chan *dirTask)
		wg      sync.WaitGroup
	)
	fmt.Fprintf(stdout, "загружаю %d файлов из %d папок в %s, потоков: %d\n", len(todo), len(dirs), cfg.url, cfg.workers)
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Папка обрабатывается одним потоком по порядку: первый файл без docgroup получает от сервера
			// новую группу, остальные файлы папки загружаются уже с её номером.
			for d := range jobs {
				for _, p := range d.pairs {
					if ctx.Err() != nil {
						return
					}
					id, group, size, status, err := upload(ctx, client, cfg, p, d.group)
					name := rel(cfg.dir, p.img)
					mu.Lock()
					if err != nil {
						s.failed++
						s.byStatus[status]++
						s.failedList = append(s.failedList, name)
						fmt.Fprintf(stderr, "ОШИБКА %s: %v\n", name, err)
					} else {
						d.group = group
						s.ok++
						s.bytes += size
						line, _ := json.Marshal(logRec{Img: name, ID: id, DocGroup: group})
						_, _ = logf.Write(append(line, '\n'))
					}
					mu.Unlock()
					if n := counter.Add(1); n%100 == 0 {
						fmt.Fprintf(stdout, "  обработано %d из %d (%.1f файл/с)\n", n, len(todo), float64(n)/time.Since(start).Seconds())
					}
				}
			}
		}()
	}
feed:
	for _, d := range dirs {
		select {
		case jobs <- d:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	s.elapsed = time.Since(start)
	printSummary(stdout, s, ctx.Err() != nil)
	return s, nil
}

// findPairs обходит дерево и сопоставляет <имя>.jpg с <имя>.json в той же папке.
func findPairs(root string, s *summary) ([]pair, error) {
	images := map[string]string{} // путь без расширения (в нижнем регистре) -> путь к jpg
	metas := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		key := strings.ToLower(strings.TrimSuffix(path, filepath.Ext(path)))
		switch ext {
		case ".jpg", ".jpeg":
			images[key] = path
		case ".json":
			metas[key] = path
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var pairs []pair
	for key, img := range images {
		if m, ok := metas[key]; ok {
			pairs = append(pairs, pair{img: img, meta: m})
		} else {
			s.noJSON = append(s.noJSON, img)
		}
	}
	for key, m := range metas {
		if _, ok := images[key]; !ok {
			s.noImage = append(s.noImage, m)
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].img < pairs[j].img })
	sort.Strings(s.noJSON)
	sort.Strings(s.noImage)
	return pairs, nil
}

// dirTask - файлы одной папки: у них общий docgroup.
type dirTask struct {
	dir   string
	pairs []pair
	group int64 // 0 - группы ещё нет: её выдаст сервер при загрузке первого файла
}

// groupByDir раскладывает пары по папкам, сохраняя порядок. known - уже известные группы папок (из журнала).
func groupByDir(root string, pairs []pair, known map[string]int64) []*dirTask {
	var out []*dirTask
	byDir := map[string]*dirTask{}
	for _, p := range pairs {
		dir := slashpath.Dir(rel(root, p.img))
		d := byDir[dir]
		if d == nil {
			d = &dirTask{dir: dir, group: known[dir]}
			byDir[dir] = d
			out = append(out, d)
		}
		d.pairs = append(d.pairs, p)
	}
	return out
}

// upload отправляет одну пару. group > 0 - добавить скан в существующую группу, 0 - выдать новую.
// Возвращает id, номер группы и размер картинки; при ошибке - причину (код ответа или "сеть").
func upload(ctx context.Context, client *http.Client, cfg config, p pair, group int64) (id string, docGroup, size int64, status string, err error) {
	meta, err := os.ReadFile(p.meta)
	if err != nil {
		return "", 0, 0, "чтение", err
	}
	meta = bytes.TrimPrefix(meta, []byte("\xef\xbb\xbf")) // BOM сервер не примет: это не JSON
	img, err := os.Open(p.img)
	if err != nil {
		return "", 0, 0, "чтение", err
	}
	defer img.Close()
	info, err := img.Stat()
	if err != nil {
		return "", 0, 0, "чтение", err
	}

	// тело собирается потоком: картинка не читается в память целиком
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(func() error {
			if err := mw.WriteField("meta", string(meta)); err != nil {
				return err
			}
			if group > 0 {
				if err := mw.WriteField("docgroup", strconv.FormatInt(group, 10)); err != nil {
					return err
				}
			}
			fw, err := mw.CreateFormFile("file", filepath.Base(p.img))
			if err != nil {
				return err
			}
			if _, err := io.Copy(fw, img); err != nil {
				return err
			}
			return mw.Close()
		}())
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.url, "/")+"/scans", pr)
	if err != nil {
		pr.Close()
		return "", 0, 0, "сеть", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, "сеть", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Detail == "" {
			e.Detail = strings.TrimSpace(string(body))
		}
		return "", 0, 0, fmt.Sprint(resp.StatusCode), fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Detail)
	}
	var ok struct {
		ID       string `json:"id"`
		DocGroup int64  `json:"docgroup"`
	}
	if err := json.Unmarshal(body, &ok); err != nil || ok.ID == "" || ok.DocGroup < 1 {
		return "", 0, 0, "ответ", fmt.Errorf("HTTP 201, но в ответе нет id или docgroup: %s", body)
	}
	if group > 0 && ok.DocGroup != group {
		return "", 0, 0, "ответ", fmt.Errorf("сервер вернул docgroup %d вместо %d", ok.DocGroup, group)
	}
	return ok.ID, ok.DocGroup, info.Size(), "", nil
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

// logRec - строка журнала загрузки.
type logRec struct {
	Img      string `json:"img"` // путь от -dir, разделитель "/"
	ID       string `json:"id"`
	DocGroup int64  `json:"docgroup,omitempty"`
}

// readLog читает журнал: какие картинки уже загружены и в какую группу.
func readLog(path string) (map[string]logRec, error) {
	done := map[string]logRec{}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var rec logRec
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Img != "" {
			done[rec.Img] = rec
		}
	}
	return done, sc.Err()
}

func printSummary(w io.Writer, s *summary, interrupted bool) {
	if interrupted {
		fmt.Fprintln(w, "прервано пользователем")
	}
	fmt.Fprintf(w, "готово за %s: загружено %d, ошибок %d, отправлено %.1f МБ\n",
		s.elapsed.Round(time.Millisecond), s.ok, s.failed, float64(s.bytes)/(1<<20))
	if len(s.byStatus) > 0 {
		var keys []string
		for k := range s.byStatus {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "  отказов %s: %d\n", k, s.byStatus[k])
		}
	}
}
