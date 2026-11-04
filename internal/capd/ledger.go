package capd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the release stays one static binary

	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

// Scope is one budget a call counts against.
type Scope struct {
	ID       string
	Cap      money.Pico
	MaxSteps int64 // 0 means no limit
}

// Decision is the outcome of a reservation.
type Decision int

// Decisions.
const (
	Admitted  Decision = iota
	Exhausted          // the budget is spent; retrying will not help
	HeldBack           // in-flight holds fill the budget; it may free up
	StepLimit          // the run has made too many calls
)

// Balance is one budget's current state.
type Balance struct {
	Cap, Spent, Held money.Pico
	Steps            int64
}

// ErrSettled means a hold was already resolved. Settling twice would corrupt the books.
var ErrSettled = errors.New("hold already settled")

const maxBatch = 128

type op struct {
	apply func(*sql.Tx) error
	done  chan error
}

// Ledger is the durable record of spend. One goroutine owns all writes and groups
// waiting calls into one transaction, so SQLite's single writer serialises admissions
// and an fsync is shared across a burst. A caller is released only after commit.
type Ledger struct {
	db   *sql.DB
	ops  chan *op
	stop chan struct{}
	done chan struct{}
	now  func() time.Time
}

// Open opens the ledger and resolves holds left by a crash. Their calls may already
// have been billed, so each is charged in full rather than released. It returns how
// many there were.
func Open(path, synchronous string) (*Ledger, int, error) {
	if synchronous != "FULL" && synchronous != "NORMAL" {
		return nil, 0, fmt.Errorf("synchronous must be FULL or NORMAL, got %q", synchronous)
	}
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(" + synchronous + ")"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, 0, err
	}
	db.SetMaxOpenConns(1)
	l := &Ledger{db: db, ops: make(chan *op, maxBatch*4), stop: make(chan struct{}), done: make(chan struct{}), now: time.Now}
	recovered, err := l.prepare()
	if err != nil {
		db.Close()
		return nil, 0, err
	}
	go l.run()
	return l, recovered, nil
}

func (l *Ledger) prepare() (int, error) {
	if _, err := l.db.Exec(`
CREATE TABLE IF NOT EXISTS budgets(
  id TEXT PRIMARY KEY, cap INTEGER NOT NULL, spent INTEGER NOT NULL DEFAULT 0,
  held INTEGER NOT NULL DEFAULT 0, steps INTEGER NOT NULL DEFAULT 0, updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS holds(
  id INTEGER PRIMARY KEY AUTOINCREMENT, scopes TEXT NOT NULL, amount INTEGER NOT NULL,
  state TEXT NOT NULL, cost INTEGER, created INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS holds_open ON holds(state) WHERE state = 'open';`); err != nil {
		return 0, err
	}
	tx, err := l.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM holds WHERE state = 'open'`)
	if err != nil {
		return 0, err
	}
	var open []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		open = append(open, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	for _, id := range open {
		if err := l.settleTx(tx, id, 0, "captured"); err != nil {
			return 0, err
		}
	}
	return len(open), tx.Commit()
}

func (l *Ledger) run() {
	defer close(l.done)
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		select {
		case first := <-l.ops:
			batch := []*op{first}
		collect:
			for len(batch) < maxBatch {
				select {
				case next := <-l.ops:
					batch = append(batch, next)
				default:
					break collect
				}
			}
			l.commit(batch)
		case <-prune.C:
			// Idle runs would otherwise grow the file forever. The key budget is the
			// hard boundary, so forgetting an old run cannot exceed it.
			cutoff := l.now().Add(-7 * 24 * time.Hour).Unix()
			_, _ = l.db.Exec(`DELETE FROM budgets WHERE id LIKE 'run:%' AND held = 0 AND updated < ?`, cutoff)
		case <-l.stop:
			return
		}
	}
}

// commit applies a batch in one transaction. Any failure fails the whole batch, which
// fails closed: no call in it is forwarded.
func (l *Ledger) commit(batch []*op) {
	finish := func(err error) {
		for _, o := range batch {
			o.done <- err
		}
	}
	tx, err := l.db.Begin()
	if err != nil {
		finish(err)
		return
	}
	for _, o := range batch {
		if err := o.apply(tx); err != nil && !errors.Is(err, ErrSettled) {
			tx.Rollback()
			finish(err)
			return
		} else if err != nil {
			o.apply = nil // only this call fails; report it after commit
		}
	}
	if err := tx.Commit(); err != nil {
		finish(err)
		return
	}
	for _, o := range batch {
		if o.apply == nil {
			o.done <- ErrSettled
		} else {
			o.done <- nil
		}
	}
}

func (l *Ledger) submit(ctx context.Context, apply func(*sql.Tx) error) error {
	o := &op{apply: apply, done: make(chan error, 1)}
	select {
	case l.ops <- o:
	case <-ctx.Done():
		return ctx.Err()
	case <-l.done:
		return errors.New("ledger closed")
	}
	select {
	case err := <-o.done:
		return err
	case <-l.done:
		return errors.New("ledger closed")
	}
}

// Reserve durably holds amount against every scope, or none of them.
func (l *Ledger) Reserve(ctx context.Context, scopes []Scope, amount money.Pico) (int64, Decision, error) {
	if amount <= 0 || len(scopes) == 0 {
		return 0, 0, errors.New("a hold needs a positive amount and a scope")
	}
	var id int64
	decision := Admitted
	err := l.submit(ctx, func(tx *sql.Tx) error {
		id, decision = 0, Admitted
		now := l.now().Unix()
		for _, s := range scopes {
			if _, err := tx.Exec(`INSERT INTO budgets(id, cap, updated) VALUES(?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET cap = excluded.cap`, s.ID, int64(s.Cap), now); err != nil {
				return err
			}
			b, err := balance(tx, s.ID)
			if err != nil {
				return err
			}
			switch {
			case s.MaxSteps > 0 && b.Steps >= s.MaxSteps:
				decision = StepLimit
			case money.Add(b.Spent, amount) > s.Cap:
				decision = Exhausted
			case money.Add(money.Add(b.Spent, b.Held), amount) > s.Cap:
				decision = HeldBack
			}
			if decision != Admitted {
				return nil
			}
		}
		ids := make([]string, len(scopes))
		for i, s := range scopes {
			ids[i] = s.ID
			if _, err := tx.Exec(`UPDATE budgets SET held = held + ?, steps = steps + 1, updated = ? WHERE id = ?`, int64(amount), now, s.ID); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO holds(scopes, amount, state, created) VALUES(?, ?, 'open', ?)`, string(raw), int64(amount), now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return id, decision, nil
}

// Settle charges the true cost of a finished call.
func (l *Ledger) Settle(ctx context.Context, id int64, cost money.Pico) error {
	return l.submit(ctx, func(tx *sql.Tx) error { return l.settleTx(tx, id, cost, "settled") })
}

// Capture charges the whole hold, for a call whose cost is unknown. Unlike a card
// hold it never lapses, because the provider may already have billed.
func (l *Ledger) Capture(ctx context.Context, id int64) error {
	return l.submit(ctx, func(tx *sql.Tx) error { return l.settleTx(tx, id, 0, "captured") })
}

// Release frees a hold for a call the provider refused without billing.
func (l *Ledger) Release(ctx context.Context, id int64) error {
	return l.submit(ctx, func(tx *sql.Tx) error { return l.settleTx(tx, id, 0, "released") })
}

func (l *Ledger) settleTx(tx *sql.Tx, id int64, cost money.Pico, state string) error {
	var raw, current string
	var amount int64
	err := tx.QueryRow(`SELECT scopes, amount, state FROM holds WHERE id = ?`, id).Scan(&raw, &amount, &current)
	if err != nil {
		return err
	}
	if current != "open" {
		return ErrSettled
	}
	switch state {
	case "captured":
		cost = money.Pico(amount)
	case "released":
		cost = 0
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return err
	}
	now := l.now().Unix()
	for _, scope := range ids {
		b, err := balance(tx, scope)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE budgets SET spent = ?, held = held - ?, updated = ? WHERE id = ?`,
			int64(money.Add(b.Spent, cost)), amount, now, scope); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE holds SET state = ?, cost = ? WHERE id = ?`, state, int64(cost), id)
	return err
}

type querier interface {
	QueryRow(string, ...any) *sql.Row
}

func balance(q querier, id string) (Balance, error) {
	var b Balance
	var limit, spent, held int64
	err := q.QueryRow(`SELECT cap, spent, held, steps FROM budgets WHERE id = ?`, id).Scan(&limit, &spent, &held, &b.Steps)
	b.Cap, b.Spent, b.Held = money.Pico(limit), money.Pico(spent), money.Pico(held)
	return b, err
}

// Balance reads one budget. A budget not used yet reads as zero.
func (l *Ledger) Balance(ctx context.Context, id string) (Balance, error) {
	var b Balance
	err := l.submit(ctx, func(tx *sql.Tx) error {
		var err error
		b, err = balance(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			b, err = Balance{}, nil
		}
		return err
	})
	return b, err
}

// Close stops the writer and closes the database.
func (l *Ledger) Close() error {
	close(l.stop)
	<-l.done
	return l.db.Close()
}
