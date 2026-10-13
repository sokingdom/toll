// Package store keeps recent requests in an embedded SQLite database for the
// dashboard.
//
// The store is a disposable copy of the event stream, not the record of
// truth: the proxy never reads it, writes are batched off the request path,
// and rows older than the retention period are deleted. Billing should use
// the event files.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sokingdom/toll/internal/events"
	"github.com/sokingdom/toll/internal/reqid"
)

const (
	schemaVersion = "1"
	flushInterval = time.Second
	pruneInterval = 10 * time.Minute
	pruneBatch    = 5000
	// maxPending bounds the rows waiting for the next flush. When the
	// database cannot keep up, further rows are dropped and counted.
	maxPending = 50_000
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS requests (
	request_id         TEXT PRIMARY KEY,
	started_at         INTEGER NOT NULL,
	user_id            TEXT,
	key_id             TEXT,
	key_hint           TEXT,
	provider           TEXT NOT NULL,
	endpoint           TEXT NOT NULL,
	requested_model    TEXT,
	model              TEXT,
	tags               TEXT,
	stream             INTEGER NOT NULL,
	status             INTEGER,
	rejected           INTEGER NOT NULL DEFAULT 0,
	error              TEXT,
	partial            INTEGER NOT NULL DEFAULT 0,
	usage_source       TEXT,
	input_tokens       INTEGER,
	cache_read_tokens  INTEGER,
	cache_write_tokens INTEGER,
	output_tokens      INTEGER,
	reasoning_tokens   INTEGER,
	latency_ms         INTEGER,
	ttfb_ms            INTEGER
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS requests_user ON requests (user_id, request_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS requests_open ON requests (request_id) WHERE status IS NULL;
`

// Request is one row: a request that has started and possibly completed.
// Status is nil until the request completes.
type Request struct {
	RequestID      string            `json:"request_id"`
	StartedAt      int64             `json:"started_at"` // unix ms
	User           *string           `json:"user"`
	KeyID          *string           `json:"key_id"`
	KeyHint        *string           `json:"key_hint"`
	Provider       string            `json:"provider"`
	Endpoint       string            `json:"endpoint"`
	RequestedModel *string           `json:"requested_model"`
	Model          *string           `json:"model"`
	Tags           map[string]string `json:"tags"`
	Stream         bool              `json:"stream"`
	Status         *int              `json:"status"`
	Rejected       bool              `json:"rejected"`
	Error          *string           `json:"error"`
	Partial        bool              `json:"partial"`
	UsageSource    *string           `json:"usage_source"`
	Tokens
	LatencyMS *int64 `json:"latency_ms"`
	TTFBMS    *int64 `json:"ttfb_ms"`
	// KeyAlias is not stored; readers fill it from the current key_aliases.
	KeyAlias *string `json:"key_alias"`
}

type Tokens struct {
	InputTokens      *int64 `json:"input_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	ReasoningTokens  *int64 `json:"reasoning_tokens"`
}

type op struct {
	completed bool
	row       Request
}

// Store batches writes to SQLite and answers dashboard queries.
type Store struct {
	db        *sql.DB
	path      string
	retention time.Duration
	log       *slog.Logger
	now       func() time.Time

	mu      sync.Mutex
	pending []op

	dropped     atomic.Int64
	writeErrors atomic.Int64
	lastErrLog  atomic.Int64

	quit chan struct{}
	done chan struct{}
}

// Open creates or opens the database at path and starts the background
// writer. Close must be called to flush the last batch.
func Open(path string, retention time.Duration, log *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		db: db, path: path, retention: retention, log: log, now: time.Now,
		quit: make(chan struct{}), done: make(chan struct{}),
	}
	go s.loop()
	return s, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	var v string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = db.Exec(`INSERT INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		return err
	case err != nil:
		return err
	case v != schemaVersion:
		return fmt.Errorf("dashboard database has schema version %s, this toll expects %s; delete the file to recreate it", v, schemaVersion)
	}
	return nil
}

// Started records a request that has been sent upstream.
func (s *Store) Started(ev events.Started, at time.Time) {
	s.enqueue(op{row: fromCommon(ev.Common, at)})
}

// Completed records the outcome of a request that started at startedAt.
func (s *Store) Completed(ev events.Completed, startedAt time.Time) {
	r := fromCommon(ev.Common, startedAt)
	status, latency := ev.Status, ev.LatencyMS
	usage := ev.UsageSource
	r.Model, r.Status, r.Rejected, r.Partial = ev.Model, &status, ev.Rejected, ev.Partial
	r.UsageSource, r.LatencyMS, r.TTFBMS = &usage, &latency, ev.TTFBMS
	if ev.Error != "" {
		r.Error = &ev.Error
	}
	r.Tokens = Tokens{ev.InputTokens, ev.CacheReadTokens, ev.CacheWriteTokens, ev.OutputTokens, ev.ReasoningTokens}
	s.enqueue(op{completed: true, row: r})
}

func fromCommon(c events.Common, startedAt time.Time) Request {
	return Request{
		RequestID: c.RequestID, StartedAt: startedAt.UnixMilli(),
		User: c.User, KeyID: c.KeyID, KeyHint: c.KeyHint,
		Provider: c.Provider, Endpoint: c.Endpoint, RequestedModel: c.RequestedModel,
		Tags: c.Tags, Stream: c.Stream,
	}
}

func (s *Store) enqueue(o op) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) >= maxPending {
		s.dropped.Add(1)
		return
	}
	s.pending = append(s.pending, o)
}

func (s *Store) loop() {
	defer close(s.done)
	flush := time.NewTicker(flushInterval)
	defer flush.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()
	s.prune()
	for {
		select {
		case <-flush.C:
			s.flush()
		case <-prune.C:
			s.prune()
		case <-s.quit:
			s.flush()
			return
		}
	}
}

// Flush writes pending rows now. It is called by the background writer and
// is exported for tests.
func (s *Store) Flush() { s.flush() }

func (s *Store) flush() {
	s.mu.Lock()
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := s.write(batch); err != nil {
		s.writeErrors.Add(int64(len(batch)))
		s.logError("dashboard store write failed", err)
	}
}

const insertStarted = `
INSERT INTO requests (request_id, started_at, user_id, key_id, key_hint,
	provider, endpoint, requested_model, tags, stream)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (request_id) DO NOTHING`

const upsertCompleted = `
INSERT INTO requests (request_id, started_at, user_id, key_id, key_hint,
	provider, endpoint, requested_model, model, tags, stream, status, rejected, error, partial,
	usage_source, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, latency_ms, ttfb_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (request_id) DO UPDATE SET
	model = excluded.model, status = excluded.status,
	rejected = excluded.rejected, error = excluded.error, partial = excluded.partial,
	usage_source = excluded.usage_source, input_tokens = excluded.input_tokens,
	cache_read_tokens = excluded.cache_read_tokens, cache_write_tokens = excluded.cache_write_tokens,
	output_tokens = excluded.output_tokens, reasoning_tokens = excluded.reasoning_tokens,
	latency_ms = excluded.latency_ms, ttfb_ms = excluded.ttfb_ms`

func (s *Store) write(batch []op) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	started, err := tx.Prepare(insertStarted)
	if err != nil {
		return err
	}
	completed, err := tx.Prepare(upsertCompleted)
	if err != nil {
		return err
	}
	for _, o := range batch {
		r := o.row
		tags, _ := json.Marshal(r.Tags)
		if !o.completed {
			_, err = started.Exec(r.RequestID, r.StartedAt, r.User, r.KeyID, r.KeyHint,
				r.Provider, r.Endpoint, r.RequestedModel, string(tags), r.Stream)
		} else {
			_, err = completed.Exec(r.RequestID, r.StartedAt, r.User, r.KeyID, r.KeyHint,
				r.Provider, r.Endpoint, r.RequestedModel, r.Model, string(tags), r.Stream, r.Status, r.Rejected, r.Error, r.Partial,
				r.UsageSource, r.InputTokens, r.CacheReadTokens, r.CacheWriteTokens, r.OutputTokens, r.ReasoningTokens, r.LatencyMS, r.TTFBMS)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// prune deletes rows older than the retention period in small batches so
// that readers are never blocked for long.
func (s *Store) prune() {
	cutoff := reqid.Floor(s.now().Add(-s.retention))
	for {
		res, err := s.db.Exec(`DELETE FROM requests WHERE request_id IN
			(SELECT request_id FROM requests WHERE request_id < ? LIMIT ?)`, cutoff, pruneBatch)
		if err != nil {
			s.logError("dashboard store prune failed", err)
			return
		}
		if n, _ := res.RowsAffected(); n < pruneBatch {
			return
		}
	}
}

func (s *Store) logError(msg string, err error) {
	now := s.now().Unix()
	if last := s.lastErrLog.Load(); now-last >= 10 && s.lastErrLog.CompareAndSwap(last, now) {
		s.log.Error(msg, "err", err)
	}
}

// Close flushes pending rows and closes the database.
func (s *Store) Close() error {
	close(s.quit)
	<-s.done
	return s.db.Close()
}

// Filter selects requests for List. Empty fields match everything.
type Filter struct {
	User, Provider, Model string
	Status                string // "ok" (2xx), "error" (anything else), or ""
	Before                string // request_id cursor: only older requests
	Since, Until          time.Time
	Limit                 int
}

const columns = `request_id, started_at, user_id, key_id, key_hint,
	provider, endpoint, requested_model, model, tags, stream, status, rejected, error, partial,
	usage_source, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, latency_ms, ttfb_ms`

// List returns completed requests, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Request, error) {
	where := []string{"status IS NOT NULL"}
	var args []any
	add := func(cond string, arg any) { where = append(where, cond); args = append(args, arg) }
	if f.User != "" {
		add("user_id = ?", f.User)
	}
	if f.Provider != "" {
		add("provider = ?", f.Provider)
	}
	if f.Model != "" {
		add("coalesce(model, requested_model) = ?", f.Model)
	}
	switch f.Status {
	case "ok":
		where = append(where, "status BETWEEN 200 AND 299")
	case "error":
		where = append(where, "(status < 200 OR status > 299)")
	}
	if f.Before != "" {
		add("request_id < ?", f.Before)
	}
	if !f.Since.IsZero() {
		add("request_id >= ?", reqid.Floor(f.Since))
	}
	if !f.Until.IsZero() {
		add("request_id < ?", reqid.Floor(f.Until))
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	q := `SELECT ` + columns + ` FROM requests WHERE ` + strings.Join(where, " AND ") + ` ORDER BY request_id DESC LIMIT ?`
	return s.query(ctx, q, append(args, f.Limit)...)
}

// Open returns requests that have no completion yet, newest first. A
// request is still in flight if this process started it; otherwise its
// completion was lost.
func (s *Store) Open(ctx context.Context, limit int) ([]Request, error) {
	return s.query(ctx, `SELECT `+columns+` FROM requests WHERE status IS NULL ORDER BY request_id DESC LIMIT ?`, limit)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Request, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		var r Request
		var tags sql.NullString
		if err := rows.Scan(&r.RequestID, &r.StartedAt, &r.User, &r.KeyID, &r.KeyHint,
			&r.Provider, &r.Endpoint, &r.RequestedModel, &r.Model, &tags,
			&r.Stream, &r.Status, &r.Rejected, &r.Error, &r.Partial, &r.UsageSource,
			&r.InputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.OutputTokens, &r.ReasoningTokens,
			&r.LatencyMS, &r.TTFBMS); err != nil {
			return nil, err
		}
		if tags.Valid {
			_ = json.Unmarshal([]byte(tags.String), &r.Tags)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageRow is the part of a request needed for per-user aggregation.
type UsageRow struct {
	StartedAt time.Time
	Model     string
	Status    int
	Tokens    [5]int64 // input, cache_read, cache_write, output, reasoning
}

// ScanUser calls fn for each completed request of user received in
// [since, until), oldest first.
func (s *Store) ScanUser(ctx context.Context, user string, since, until time.Time, fn func(UsageRow)) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT started_at, coalesce(model, requested_model, ''), coalesce(status, 0),
			coalesce(input_tokens, 0), coalesce(cache_read_tokens, 0), coalesce(cache_write_tokens, 0),
			coalesce(output_tokens, 0), coalesce(reasoning_tokens, 0)
		FROM requests
		WHERE user_id = ? AND request_id >= ? AND request_id < ? AND status IS NOT NULL
		ORDER BY user_id, request_id`, user, reqid.Floor(since), reqid.Floor(until))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r UsageRow
		var ms int64
		if err := rows.Scan(&ms, &r.Model, &r.Status, &r.Tokens[0], &r.Tokens[1], &r.Tokens[2], &r.Tokens[3], &r.Tokens[4]); err != nil {
			return err
		}
		r.StartedAt = time.UnixMilli(ms)
		fn(r)
	}
	return rows.Err()
}

// Stats describes the store for the dashboard.
type Stats struct {
	Path        string `json:"path"`
	SizeBytes   int64  `json:"size_bytes"`
	Retention   string `json:"retention"`
	Oldest      *int64 `json:"oldest"` // unix ms of the oldest kept request
	Pending     int    `json:"pending"`
	Dropped     int64  `json:"dropped"`
	WriteErrors int64  `json:"write_errors"`
}

func (s *Store) Stats(ctx context.Context) Stats {
	st := Stats{
		Path: s.path, Retention: FormatDuration(s.retention),
		Dropped: s.dropped.Load(), WriteErrors: s.writeErrors.Load(),
	}
	s.mu.Lock()
	st.Pending = len(s.pending)
	s.mu.Unlock()
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			st.SizeBytes += fi.Size()
		}
	}
	var oldest sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT min(request_id) FROM requests`).Scan(&oldest); err == nil && oldest.Valid {
		if t, ok := reqid.Time(oldest.String); ok {
			ms := t.UnixMilli()
			st.Oldest = &ms
		}
	}
	return st
}

// FormatDuration prints whole days as "31d" and anything else as Go does.
func FormatDuration(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}
