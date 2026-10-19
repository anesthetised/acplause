// Package store keeps sessions in a SQLite database shared by every
// acplause process of the user (ADR 0004). It stores rows; what they mean
// is the agent's business.
package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Lease timing: a process renews the leases of its sessions every
// LeaseRenewal; a lease not renewed for leaseTTL is abandoned.
const (
	LeaseRenewal = 10 * time.Second
	leaseTTL     = 30 * time.Second
)

// Errors of session ownership.
var (
	ErrNotFound = errors.New("session not found")
	ErrBusy     = errors.New("session is busy in another acplause process")
	ErrLost     = errors.New("session was resumed in another acplause process")
)

// idle is the foreground state in which another process may take a session
// over.
const idle = "idle"

// Session is a session's row.
type Session struct {
	ID        string
	Cwd       string
	Dirs      []string
	Title     string
	UpdatedAt time.Time
	Model     string
	Effort    string
	Cost      float64
	State     string // foreground state as last saved
}

// Row is one history or transcript entry at its position.
type Row struct {
	Seq  int
	Data []byte
}

// Loaded is a session taken over by this process, with its entries in order.
type Loaded struct {
	Session
	History    [][]byte
	Transcript [][]byte
}

// Store is an open database. Each process has one, identified by owner.
type Store struct {
	db    *sql.DB
	owner string
	now   func() time.Time
}

// File is the database's name in its directory.
const File = "acplause.db"

// Open opens or creates the database in dir, creating dir 0700 and the file
// 0600: it holds conversations in plain text. owner identifies this process
// in session leases.
func Open(dir, owner string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, File)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()

	// Immediate transactions take the write lock up front, so concurrent
	// processes queue on busy_timeout instead of failing to upgrade a read.
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, owner: owner, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrations upgrade the schema one version each; PRAGMA user_version is
// the number applied. Only append to this list.
var migrations = []string{
	`CREATE TABLE sessions (
		id          TEXT PRIMARY KEY,
		cwd         TEXT NOT NULL,
		dirs        TEXT NOT NULL,    -- JSON array
		title       TEXT NOT NULL,
		updated_at  INTEGER NOT NULL, -- Unix nanoseconds
		model       TEXT NOT NULL,
		effort      TEXT NOT NULL,
		cost        REAL NOT NULL,
		state       TEXT NOT NULL,
		owner       TEXT NOT NULL,    -- '' when no process holds it
		lease_until INTEGER NOT NULL  -- Unix nanoseconds
	);
	CREATE INDEX sessions_by_update ON sessions (updated_at DESC, id);
	CREATE TABLE history (
		session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
		seq        INTEGER NOT NULL,
		data       TEXT NOT NULL,
		PRIMARY KEY (session_id, seq)
	) WITHOUT ROWID;
	CREATE TABLE transcript (
		session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
		seq        INTEGER NOT NULL,
		data       TEXT NOT NULL,
		PRIMARY KEY (session_id, seq)
	) WITHOUT ROWID;`,
	`CREATE TABLE settings (
		id     INTEGER PRIMARY KEY CHECK (id = 1),
		model  TEXT NOT NULL,
		effort TEXT NOT NULL
	);`,
}

func (s *Store) migrate(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return err
		}
		if version > len(migrations) {
			return fmt.Errorf("database schema %d is newer than this acplause (%d); upgrade acplause", version, len(migrations))
		}
		for ; version < len(migrations); version++ {
			if _, err := tx.ExecContext(ctx, migrations[version]); err != nil {
				return fmt.Errorf("migrating to schema %d: %w", version+1, err)
			}
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version))
		return err
	})
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) leaseUntil() int64 { return s.now().Add(leaseTTL).UnixNano() }

// Selection returns the last explicit model and effort selection, or empty
// strings if the user has not selected one yet.
func (s *Store) Selection(ctx context.Context) (model, effort string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT model, effort FROM settings WHERE id = 1`).Scan(&model, &effort)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return model, effort, err
}

// SetSelection remembers the model and effort together, independently of
// the lifetime of the session in which they were selected.
func (s *Store) SetSelection(ctx context.Context, model, effort string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (id, model, effort) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET model = excluded.model, effort = excluded.effort`, model, effort)
	return err
}

// Create stores a new session owned by this process.
func (s *Store) Create(ctx context.Context, sess Session) error {
	dirs, err := json.Marshal(nonNil(sess.Dirs))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO sessions
		(id, cwd, dirs, title, updated_at, model, effort, cost, state, owner, lease_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Cwd, dirs, sess.Title, sess.UpdatedAt.UnixNano(), sess.Model, sess.Effort, sess.Cost, sess.State,
		s.owner, s.leaseUntil())
	return err
}

// Save replaces the history suffix starting at history[0].Seq and upserts
// transcript entries atomically. An empty history leaves it unchanged.
// It fails with ErrLost once another process has the session.
func (s *Store) Save(ctx context.Context, sess Session, history, transcript []Row) error {
	dirs, err := json.Marshal(nonNil(sess.Dirs))
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE sessions
			SET dirs = ?, title = ?, updated_at = ?, model = ?, effort = ?, cost = ?, state = ?
			WHERE id = ? AND owner = ?`,
			dirs, sess.Title, sess.UpdatedAt.UnixNano(), sess.Model, sess.Effort, sess.Cost, sess.State, sess.ID, s.owner)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmp.Or(err, ErrLost)
		}
		if len(history) > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM history WHERE session_id = ? AND seq >= ?`, sess.ID, history[0].Seq); err != nil {
				return err
			}
		}
		for _, t := range []struct {
			table string
			rows  []Row
		}{{"history", history}, {"transcript", transcript}} {
			for _, r := range t.rows {
				if _, err := tx.ExecContext(ctx,
					`INSERT OR REPLACE INTO `+t.table+` (session_id, seq, data) VALUES (?, ?, ?)`,
					sess.ID, r.Seq, string(r.Data)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Acquire takes a session over for this process and loads it. A session
// another live process holds can only be taken while it is idle there.
func (s *Store) Acquire(ctx context.Context, id string) (*Loaded, error) {
	var l Loaded
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var dirs string
		var updated, leaseUntil int64
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT cwd, dirs, title, updated_at, model, effort, cost, state, owner, lease_until
			FROM sessions WHERE id = ?`, id).
			Scan(&l.Cwd, &dirs, &l.Title, &updated, &l.Model, &l.Effort, &l.Cost, &l.State, &owner, &leaseUntil)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if s.heldElsewhere(owner, leaseUntil, l.State) {
			return ErrBusy
		}
		l.ID, l.UpdatedAt = id, time.Unix(0, updated)
		if err := json.Unmarshal([]byte(dirs), &l.Dirs); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET owner = ?, lease_until = ? WHERE id = ?`,
			s.owner, s.leaseUntil(), id); err != nil {
			return err
		}
		if l.History, err = entries(ctx, tx, "history", id); err != nil {
			return err
		}
		l.Transcript, err = entries(ctx, tx, "transcript", id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// heldElsewhere reports whether another live process is working on a
// session, which then cannot be taken over.
func (s *Store) heldElsewhere(owner string, leaseUntil int64, state string) bool {
	return owner != "" && owner != s.owner && leaseUntil > s.now().UnixNano() && state != idle
}

func entries(ctx context.Context, tx *sql.Tx, table, id string) ([][]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT data FROM `+table+` WHERE session_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		out = append(out, []byte(data))
	}
	return out, rows.Err()
}

// Renew extends the leases of sessions this process holds and returns those
// it no longer holds.
func (s *Store) Renew(ctx context.Context, ids []string) (lost []string, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		lost = nil
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE sessions SET lease_until = ? WHERE id = ? AND owner = ?`,
				s.leaseUntil(), id, s.owner)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				lost = append(lost, id)
			}
		}
		return nil
	})
	return lost, err
}

// Release gives up a session this process holds, so others can take it at
// once.
func (s *Store) Release(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET owner = '', lease_until = 0 WHERE id = ? AND owner = ?`, id, s.owner)
	return err
}

// Delete removes a session and its entries. Deleting an unknown session
// succeeds; one another live process is working on is ErrBusy.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var owner, state string
		var leaseUntil int64
		err := tx.QueryRowContext(ctx, `SELECT owner, lease_until, state FROM sessions WHERE id = ?`, id).
			Scan(&owner, &leaseUntil, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if s.heldElsewhere(owner, leaseUntil, state) {
			return ErrBusy
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
		return err
	})
}

// List returns up to limit sessions after offset, most recently updated
// first, optionally only those in cwd, and whether more follow.
func (s *Store) List(ctx context.Context, cwd string, offset, limit int) ([]Session, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, cwd, dirs, title, updated_at FROM sessions
		WHERE ? = '' OR cwd = ? ORDER BY updated_at DESC, id LIMIT ? OFFSET ?`, cwd, cwd, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var dirs string
		var updated int64
		if err := rows.Scan(&sess.ID, &sess.Cwd, &dirs, &sess.Title, &updated); err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal([]byte(dirs), &sess.Dirs); err != nil {
			return nil, false, err
		}
		sess.UpdatedAt = time.Unix(0, updated)
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func nonNil(dirs []string) []string {
	if dirs == nil {
		return []string{}
	}
	return dirs
}
