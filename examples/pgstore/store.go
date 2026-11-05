// Package pgstore is a graph.CheckpointStore on Postgres, written against
// database/sql only: the SDK core has no driver dependency, you bring one
// (pgx's stdlib adapter, lib/pq) and pass the *sql.DB.
//
//	db, _ := sql.Open("pgx", dsn)
//	store, _ := pgstore.New(ctx, db)
//	run, _ := g.Compile(graph.WithCheckpointStore(store))
//
// One row per record. (run_id, seq) is the primary key, so two processes
// recovering the same run collide on the insert instead of interleaving.
package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/tggo/claude-agent-go/graph"
)

const ddl = `CREATE TABLE IF NOT EXISTS graph_records (
	run_id text    NOT NULL,
	seq    integer NOT NULL,
	rec    jsonb   NOT NULL,
	PRIMARY KEY (run_id, seq)
)`

// Store implements graph.CheckpointStore.
type Store struct{ db *sql.DB }

// New creates the table if needed.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Append inserts one record; the commit is durable when the statement returns.
func (s *Store) Append(ctx context.Context, runID string, rec graph.Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO graph_records (run_id, seq, rec) VALUES ($1, $2, $3)`, runID, rec.Seq, b)
	return err
}

// Load returns the run's records in order.
func (s *Store) Load(ctx context.Context, runID string) ([]graph.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rec FROM graph_records WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Record
	for rows.Next() {
		var b []byte
		var r graph.Record
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// List summarises every run, most recently updated first. It loads every
// record; for many runs keep a summary table instead.
func (s *Store) List(ctx context.Context) ([]graph.RunSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT run_id FROM graph_records`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	now := time.Now()
	var out []graph.RunSummary
	for _, id := range ids {
		recs, err := s.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, graph.Summarize(id, recs, now))
	}
	// most recently updated first
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Updated.After(out[j-1].Updated); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

var _ graph.CheckpointStore = (*Store)(nil)
