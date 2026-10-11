package metastore

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres подключается к БД и, если migrate, создаёт таблицы.
func OpenPostgres(ctx context.Context, dsn string, maxConns int32, migrate bool) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("строка подключения к БД: %w", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("подключение к БД: %w", err)
	}
	if migrate {
		// простой протокол: несколько команд в одном Exec
		if _, err := pool.Exec(ctx, schemaSQL, pgx.QueryExecModeSimpleProtocol); err != nil {
			pool.Close()
			return nil, fmt.Errorf("создание схемы БД: %w", err)
		}
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Insert добавляет скан. Если r.DocGroup == 0, скан получает новую группу из последовательности;
// иначе группа должна уже существовать (ErrGroupNotFound). Итоговое значение записывается в r.DocGroup.
func (p *Postgres) Insert(ctx context.Context, r *Record) error {
	var group *int64 // nil -> DEFAULT-подобное поведение: COALESCE берёт nextval
	if r.DocGroup != 0 {
		var exists bool
		if err := p.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM scans WHERE docgroup = $1)`, r.DocGroup).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrGroupNotFound
		}
		group = &r.DocGroup
	}
	err := p.pool.QueryRow(ctx, `
		INSERT INTO scans (id, file, doctype, docnum, person_name, person_bday, metainfo,
		                   sha256, size_bytes, width, height, created_at, docgroup)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, COALESCE($13, nextval('scans_docgroup_seq')))
		RETURNING docgroup`,
		r.ID, r.File, r.DocType, r.DocNum, r.PersonName, r.PersonBday, string(r.MetaInfo),
		r.SHA256, r.SizeBytes, r.Width, r.Height, r.CreatedAt, group).Scan(&r.DocGroup)
	var pgErr *pgconn.PgError
	// класс 22 - data exception: jsonb не принял значение (\u0000, переполнение numeric и т.п.)
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") {
		return fmt.Errorf("%w: %s", ErrInvalidMeta, pgErr.Message)
	}
	return err
}

func (p *Postgres) Get(ctx context.Context, id uuid.UUID) (*Record, error) {
	r := Record{ID: id}
	var meta []byte
	err := p.pool.QueryRow(ctx, `
		SELECT file, doctype, docnum, person_name, person_bday, metainfo,
		       sha256, size_bytes, width, height, created_at, docgroup
		FROM scans WHERE id = $1`, id).
		Scan(&r.File, &r.DocType, &r.DocNum, &r.PersonName, &r.PersonBday, &meta,
			&r.SHA256, &r.SizeBytes, &r.Width, &r.Height, &r.CreatedAt, &r.DocGroup)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.MetaInfo = meta // NULL в колонке -> nil
	return &r, nil
}

// ListGroup читает всю группу одним запросом по индексу scans_docgroup_idx.
func (p *Postgres) ListGroup(ctx context.Context, docgroup int64) ([]Record, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, file, doctype, docnum, person_name, person_bday, metainfo,
		       sha256, size_bytes, width, height, created_at
		FROM scans WHERE docgroup = $1 ORDER BY id`, docgroup)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r := Record{DocGroup: docgroup}
		var meta []byte
		if err := rows.Scan(&r.ID, &r.File, &r.DocType, &r.DocNum, &r.PersonName, &r.PersonBday, &meta,
			&r.SHA256, &r.SizeBytes, &r.Width, &r.Height, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.MetaInfo = meta
		out = append(out, r)
	}
	return out, rows.Err()
}
