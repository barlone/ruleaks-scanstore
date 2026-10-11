-- Схема хранилища метаданных; выполняется при старте сервера ([database] migrate = true).
-- Только идемпотентные команды: повторный запуск ничего не меняет.

CREATE TABLE IF NOT EXISTS scans (
    id          uuid    NOT NULL,   -- uuid7, выдаёт сервер
    file        varchar NOT NULL,   -- путь к картинке относительно [store].dir, разделитель "/"
    doctype     varchar NOT NULL,   -- "type" из JSON клиента (passport, snils, drivercard, ...)
    docnum      varchar,            -- номер документа без пробелов, тире и скобок
    person_name varchar,            -- полное имя персоны
    person_bday date,               -- день рождения персоны
    metainfo    jsonb,              -- остальной JSON клиента
    CONSTRAINT scans_pk     PRIMARY KEY (id),
    CONSTRAINT scans_unique UNIQUE (file)
);

-- Служебные поля загрузки. Добавляются отдельно, чтобы дополнить уже созданную таблицу.
ALTER TABLE scans ADD COLUMN IF NOT EXISTS sha256        text        NOT NULL; -- hex, 64 символа
ALTER TABLE scans ADD COLUMN IF NOT EXISTS size_bytes    bigint      NOT NULL;
ALTER TABLE scans ADD COLUMN IF NOT EXISTS width         integer     NOT NULL;
ALTER TABLE scans ADD COLUMN IF NOT EXISTS height        integer     NOT NULL;
ALTER TABLE scans ADD COLUMN IF NOT EXISTS created_at    timestamptz NOT NULL;

-- Группа сканов одной папки. Номер выдаёт сервер (новый скан без docgroup получает новую группу),
-- остальные сканы папки добавляются в неё по этому номеру. Для уже существующих строк
-- DEFAULT даёт каждой своё значение, то есть каждая становится отдельной группой.
CREATE SEQUENCE IF NOT EXISTS scans_docgroup_seq;
ALTER TABLE scans ADD COLUMN IF NOT EXISTS docgroup bigint NOT NULL DEFAULT nextval('scans_docgroup_seq');
ALTER SEQUENCE scans_docgroup_seq OWNED BY scans.docgroup;

CREATE INDEX IF NOT EXISTS scans_docgroup_idx ON scans (docgroup);
CREATE INDEX IF NOT EXISTS scans_type_idx   ON scans (doctype);
CREATE INDEX IF NOT EXISTS scans_docnum_idx ON scans (docnum);

-- Поиск по содержимому metainfo (metainfo @> '{"organization": "..."}').
CREATE INDEX IF NOT EXISTS scans_metainfo_idx ON scans USING gin (metainfo jsonb_path_ops);
