# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Общение с пользователем и комментарии в коде — на русском. Среда: Windows, Go 1.27. Git нет.

## Назначение

REST API хранилища сканов документов водителей. Это замена прототипа `D:\PyProjs\DocScanSort\docstoreapi.py` (контекст — в `D:\PyProjs\DocScanSort\CLAUDE.md`). Картинки JPEG хранятся в папке `store`, метаданные — в PostgreSQL (таблица `scans`: путь `file`, `doctype`, `docnum`, `person_name`, `person_bday` и остаток клиентского JSON в `metainfo jsonb`, плюс служебные sha256/размеры/created_at), настройки — в `config.ini` (шаблон — `config.example.ini`). Внешняя база водителей хранит только uuid сканов. API и формат ответов (`{"detail": ...}` для ошибок, `created_at` вида `...+00:00`) совместимы с прототипом. Описание API — в `README.md`.

## Команды

```
go test ./...                                     # без БД; тест Postgres - только с $env:SCANSTORE_TEST_DSN
go build -o scanstore.exe . ; .\scanstore.exe -config config.ini
.\scanstore.exe -gen-token                        # случайный api_token
go run ./cmd/bulkload -dry-run                    # массовая загрузка сканов через API (см. README); журнал bulkload.jsonl
```

Развёртывание на Linux — `deploy/` (unit-файл systemd и скрипты `start.sh`, `stop.sh`, `status.sh`, см. README). systemd на этой машине нет: скрипты проверялись только на заглушках `systemctl`, unit-файл против живого systemd не запускался.

PostgreSQL на этой машине нет (ни в Windows, ни в WSL), поэтому `internal/metastore/postgres.go` против живой базы не проверялся. Для ручной проверки сервера используйте отдельный ini-файл с другими `port` и `[store] dir`.

## Устройство

- `main.go` — конфиг, открытие хранилищ, `http.Server` с таймаутами и корректной остановкой.
- `internal/config` — разбор ini (`gopkg.in/ini.v1`, комментарии только на отдельной строке: `;`/`#` бывают в паролях). Относительный `store.dir` считается от папки ini-файла. DSN собирается из полей `[database]` (или берётся `dsn` целиком).
- `internal/filestore` — единственное место, где вычисляются пути: `store/ГГГГ/ММ/<uuid>.jpg` по времени из uuid7. `Rel(id)` даёт путь для `scans.file` (с `/`), `Abs(rel)` проверяет значение из БД и превращает его в полный путь. Запись атомарная через `store/tmp`.
- `internal/metastore` — интерфейс `Store` (Insert/Get/ListGroup/Ping), реализация `Postgres` (pgx/v5, схема `schema.sql` встроена через embed и выполняется при старте) и `Memory` для тестов. Ошибки класса `22` из БД превращаются в `ErrInvalidMeta` (ответ 422).
- `internal/api` — обработчики на стандартном `net/http.ServeMux` (маршруты вида `GET /scans/{id}`). Multipart читается потоком с лимитами, без временных файлов.

Инварианты, которые легко нарушить:

- **Порядок записи:** сначала картинка (`filestore.Put`), потом строка в БД. Скан существует, только когда есть строка. Если вставка упала, картинка удаляется. Строка есть, а файла нет — это 500 (потеря данных), а не 404.
- **`meta` нормализуется** в `parseMeta` (объект, без `\u0000`, числа как `json.Number`), затем `splitMeta` (`internal/api/docmeta.go`) раскладывает его по колонкам. В `metainfo` уходит перекодированный остаток, а не сырые байты клиента. Поле из `metainfo` убирается, только если значение в колонке его не теряет (номера с буквами/разделителями остаются). Без строкового `type` — 422.
- **`docgroup` (группа сканов одной папки):** номер выдаёт только сервер (`scans_docgroup_seq`). `POST /scans` без поля `docgroup` создаёт новую группу, с полем — добавляет в существующую (иначе 422, `metastore.ErrGroupNotFound`). Клиент берёт номер из ответа на первый скан папки; `cmd/bulkload` делает это сам (папка = один поток, группа пишется в журнал). Все сканы группы отдаёт `GET /groups/{docgroup}` (`Store.ListGroup`, запрос по индексу `scans_docgroup_idx`, порядок по `id`).
- **Схема:** `schema.sql` выполняется при старте и идемпотентно доводит таблицу `scans` (созданную вручную) до нужного вида: имена ограничений и индексов (`scans_pk`, `scans_unique`, `scans_type_idx`, `scans_docnum_idx`) взяты из неё. GIN-индекс `scans_metainfo_idx` (`jsonb_path_ops`) создаёт сам `schema.sql`.
- **Проверка JPEG:** сначала `DecodeConfig` и лимит пикселей, затем полное `jpeg.Decode` под семафором `decodeSlots`. Файл хранится в исходном виде. Все 1654 JPEG из `DocScanSort/Output` и `SampleData` стандартный декодер Go принимает.
- Токен сравнивается по sha256 через `subtle.ConstantTimeCompare`, сервер не стартует с токеном короче 16 символов.
