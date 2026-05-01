// Package store holds the only copy of who owns a run. Every write that
// belongs to an attempt carries its epoch, and a write whose epoch is no
// longer current changes nothing and returns ErrFenced.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

var (
	ErrFenced   = errors.New("attempt is no longer current")
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store struct{ DB *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	for i := 0; ; i++ {
		if err = p.Ping(ctx); err == nil {
			break
		}
		if i == 30 || ctx.Err() != nil {
			return nil, err
		}
		time.Sleep(time.Second)
	}
	// The gate and the control process start together, so migrations take a lock.
	_, err = p.Exec(ctx, "BEGIN; SELECT pg_advisory_xact_lock(7342); "+schema+" COMMIT;")
	return &Store{p}, err
}

type Run struct {
	ID          string          `json:"id"`
	Repo        string          `json:"repo"`
	AgentCmd    string          `json:"-"`
	Title       string          `json:"title"`
	State       string          `json:"state"`
	Epoch       int             `json:"epoch"`
	MaxAttempts int             `json:"max_attempts"`
	BaseSHA     string          `json:"base_sha"`
	HeadSHA     string          `json:"head_sha"`
	ApprovedSHA string          `json:"approved_sha"`
	PR          int             `json:"pr"`
	Code        string          `json:"code"`
	Owner       string          `json:"owner"`
	Detail      json.RawMessage `json:"detail"`
}

func (r *Run) Branch() string { return "refs/heads/agent/" + r.ID }

const runCols = `id, repo, agent_cmd, title, state, epoch, max_attempts, base_sha, head_sha, approved_sha, pr_number, code, owner, detail`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Repo, &r.AgentCmd, &r.Title, &r.State, &r.Epoch, &r.MaxAttempts,
		&r.BaseSHA, &r.HeadSHA, &r.ApprovedSHA, &r.PR, &r.Code, &r.Owner, &r.Detail)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

func (s *Store) CreateRun(ctx context.Context, repo, title, cmd string) (*Run, error) {
	return scanRun(s.DB.QueryRow(ctx,
		`INSERT INTO runs (id, repo, title, agent_cmd) VALUES ($1,$2,$3,$4) RETURNING `+runCols,
		randHex(6), repo, title, cmd))
}

func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.DB.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id=$1`, id))
}

// Claim takes the oldest queued run, or one whose worker stopped renewing.
// Taking it raises the epoch, and that alone makes every earlier sandbox's
// token useless. SKIP LOCKED matters here: a push holds the run's row lock, so
// a run cannot change hands halfway through one.
func (s *Store) Claim(ctx context.Context, worker string, lease time.Duration) (*Run, error) {
	var out *Run
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, `
			UPDATE runs SET state='running', epoch=epoch+1, claimed_by=$1, claim_until=now()+$2::interval
			WHERE id = (SELECT id FROM runs
			            WHERE state='queued' OR (state='running' AND claim_until < now())
			            ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
			RETURNING `+runCols, worker, lease.String()))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sandboxes SET revoked=true, wanted=false WHERE run_id=$1 AND epoch<$2`, r.ID, r.Epoch); err != nil {
			return err
		}
		if r.Epoch > r.MaxAttempts {
			r.State, r.Code, r.Owner = "failed", "attempts_exhausted", "platform"
			if _, err := tx.Exec(ctx, `UPDATE runs SET state='failed', code=$2, owner=$3 WHERE id=$1`, r.ID, r.Code, r.Owner); err != nil {
				return err
			}
		}
		out = r
		return addEvent(ctx, tx, r.ID, r.Epoch, "claimed", map[string]any{"worker": worker, "state": r.State})
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return out, err
}

func (s *Store) Renew(ctx context.Context, r *Run, lease time.Duration) error {
	return s.guarded(ctx, `UPDATE runs SET claim_until=now()+$3::interval WHERE id=$1 AND epoch=$2 AND state='running'`, r.ID, r.Epoch, lease.String())
}

func (s *Store) SetBase(ctx context.Context, r *Run, sha string) error {
	return s.guarded(ctx, `UPDATE runs SET base_sha=$3 WHERE id=$1 AND epoch=$2 AND state='running'`, r.ID, r.Epoch, sha)
}

func (s *Store) SetPR(ctx context.Context, r *Run, n int) error {
	return s.guarded(ctx, `UPDATE runs SET pr_number=$3 WHERE id=$1 AND epoch=$2 AND state='running'`, r.ID, r.Epoch, n)
}

// Review ends the attempt. head is part of the guard so a run can only be
// shown to a reviewer at the commit that was verified.
func (s *Store) Review(ctx context.Context, r *Run, head string, keep time.Duration) error {
	return s.guarded(ctx, `UPDATE runs SET state='review', review_until=now()+$4::interval, claim_until=NULL
		WHERE id=$1 AND epoch=$2 AND state='running' AND head_sha=$3`, r.ID, r.Epoch, head, keep.String())
}

func (s *Store) Fail(ctx context.Context, r *Run, code, owner string, detail any) error {
	b, _ := json.Marshal(detail)
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		t, err := tx.Exec(ctx, `UPDATE runs SET state='failed', code=$3, owner=$4, detail=$5 WHERE id=$1 AND epoch=$2 AND state='running'`, r.ID, r.Epoch, code, owner, b)
		if err != nil {
			return err
		}
		if t.RowsAffected() == 0 {
			return ErrFenced
		}
		_, err = tx.Exec(ctx, `UPDATE sandboxes SET revoked=true, wanted=false WHERE run_id=$1`, r.ID)
		return err
	})
}

// Requeue hands the run to the next attempt without waiting for the lease.
func (s *Store) Requeue(ctx context.Context, r *Run) error {
	return s.guarded(ctx, `UPDATE runs SET state='queued', claim_until=NULL WHERE id=$1 AND epoch=$2 AND state='running'`, r.ID, r.Epoch)
}

// Cancel raises the epoch like a takeover does, so it needs no help from the
// worker that is running the attempt.
func (s *Store) Cancel(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		t, err := tx.Exec(ctx, `UPDATE runs SET state='cancelled', epoch=epoch+1 WHERE id=$1 AND state IN ('queued','running','review','attention')`, id)
		if err != nil {
			return err
		}
		if t.RowsAffected() == 0 {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE sandboxes SET revoked=true, wanted=false WHERE run_id=$1`, id)
		return err
	})
}

// Approve is bound to a commit. If the reviewer looked at anything else the
// call conflicts and they look again.
func (s *Store) Approve(ctx context.Context, id, sha string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		t, err := tx.Exec(ctx, `UPDATE runs SET state='approved', approved_sha=$2 WHERE id=$1 AND state='review' AND head_sha=$2 AND head_sha<>''`, id, sha)
		if err != nil {
			return err
		}
		if t.RowsAffected() == 0 {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE sandboxes SET revoked=true, wanted=false WHERE run_id=$1`, id)
		return err
	})
}

func (s *Store) guarded(ctx context.Context, sql string, args ...any) error {
	t, err := s.DB.Exec(ctx, sql, args...)
	if err == nil && t.RowsAffected() == 0 {
		return ErrFenced
	}
	return err
}

// ---- sandboxes

type Sandbox struct {
	Name, RunID, Role, Provider, Ref, State, Phase string
	Epoch                                          int
	Wanted                                         bool
	Age                                            time.Duration
}

// Intend writes the ledger row before anything exists. The run row is locked
// while we insert, so an attempt that has been replaced cannot add one.
func (s *Store) Intend(ctx context.Context, r *Run, role, provider string, caps []string) (name, token string, err error) {
	name = SandboxName(r.ID, r.Epoch, role)
	token = randHex(24)
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT epoch=$2 AND state='running' FROM runs WHERE id=$1 FOR SHARE`, r.ID, r.Epoch).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrFenced
		}
		_, err := tx.Exec(ctx, `INSERT INTO sandboxes (name, run_id, epoch, role, provider, token_hash, caps) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			name, r.ID, r.Epoch, role, provider, hash(token), caps)
		return err
	})
	return
}

func SandboxName(run string, epoch int, role string) string {
	return "fenced-" + run + "-" + itoa(epoch) + "-" + role
}

func (s *Store) Live(ctx context.Context, name, ref string) error {
	// A ref is recorded even for a fenced attempt: the reconciler needs it.
	_, err := s.DB.Exec(ctx, `UPDATE sandboxes SET ref=$2, state='live' WHERE name=$1 AND state='intent'`, name, ref)
	return err
}

func (s *Store) SetEndpoint(ctx context.Context, name string, ep any) error {
	b, _ := json.Marshal(ep)
	_, err := s.DB.Exec(ctx, `UPDATE sandboxes SET endpoint=$2 WHERE name=$1`, name, b)
	return err
}

func (s *Store) SetPhase(ctx context.Context, name, phase string) error {
	_, err := s.DB.Exec(ctx, `UPDATE sandboxes SET phase=$2 WHERE name=$1`, name, phase)
	return err
}

// Revoke kills the token and reports once no request can still be using it.
func (s *Store) Revoke(ctx context.Context, name string) error {
	_, err := s.DB.Exec(ctx, `UPDATE sandboxes SET revoked=true WHERE name=$1`, name)
	return err
}

func (s *Store) Unwant(ctx context.Context, name string) error {
	_, err := s.DB.Exec(ctx, `UPDATE sandboxes SET revoked=true, wanted=false WHERE name=$1`, name)
	return err
}

func (s *Store) Destroyed(ctx context.Context, name string) error {
	_, err := s.DB.Exec(ctx, `UPDATE sandboxes SET state='destroyed', revoked=true, wanted=false WHERE name=$1`, name)
	return err
}

// Open lists sandboxes that should not be forgotten. Wanted is recomputed from
// the run, so a missed update cannot keep a sandbox alive.
func (s *Store) Open(ctx context.Context) (map[string]Sandbox, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT s.name, s.run_id, s.epoch, s.role, s.provider, s.ref, s.state, s.phase,
		       s.wanted AND s.epoch=r.epoch AND (r.state='running' OR (r.state='review' AND r.review_until>now())),
		       EXTRACT(epoch FROM now()-s.created_at)::float8
		FROM sandboxes s JOIN runs r ON r.id=s.run_id WHERE s.state<>'destroyed'`)
	if err != nil {
		return nil, err
	}
	out := map[string]Sandbox{}
	var sb Sandbox
	var age float64
	_, err = pgx.ForEachRow(rows, []any{&sb.Name, &sb.RunID, &sb.Epoch, &sb.Role, &sb.Provider, &sb.Ref, &sb.State, &sb.Phase, &sb.Wanted, &age}, func() error {
		sb.Age = time.Duration(age * float64(time.Second))
		out[sb.Name] = sb
		return nil
	})
	return out, err
}

// ---- tokens

type Grant struct {
	Sandbox, Repo, RunID, Phase string
	Epoch                       int
	Caps                        []string
}

func (g *Grant) Can(c string) bool {
	for _, x := range g.Caps {
		if x == c {
			return true
		}
	}
	return false
}

func (s *Store) Grant(ctx context.Context, token string) (*Grant, error) {
	var g Grant
	err := s.DB.QueryRow(ctx, `
		SELECT s.name, r.repo, r.id, s.epoch, s.caps, s.phase FROM sandboxes s JOIN runs r ON r.id=s.run_id
		WHERE s.token_hash=$1 AND NOT s.revoked AND s.epoch=r.epoch AND r.state='running'`, hash(token)).
		Scan(&g.Sandbox, &g.Repo, &g.RunID, &g.Epoch, &g.Caps, &g.Phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFenced
	}
	return &g, err
}

// Push runs fn while holding the run's row lock. fn gets the head the branch is
// recorded at and returns the head it left the branch at.
//
// The epoch, the token and the old head are all read under that lock. Nothing
// that raises the epoch can commit until fn returns, which is why the caller
// gives fn a timeout shorter than a lease.
func (s *Store) Push(ctx context.Context, g *Grant, fn func(run *Run) (newHead string, err error)) error {
	var inner error
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id=$1 FOR UPDATE`, g.RunID))
		if err != nil {
			return err
		}
		var revoked bool
		if err := tx.QueryRow(ctx, `SELECT revoked FROM sandboxes WHERE name=$1`, g.Sandbox).Scan(&revoked); err != nil {
			return err
		}
		if revoked || r.Epoch != g.Epoch || r.State != "running" {
			return ErrFenced
		}
		head, err := fn(r)
		inner = err
		if errors.Is(err, ErrUnknown) {
			// We could not learn where the branch is. A person decides.
			_, e := tx.Exec(ctx, `UPDATE runs SET state='attention', epoch=epoch+1, code='push_unknown', owner='platform' WHERE id=$1`, r.ID)
			return e
		}
		if head == "" || head == r.HeadSHA {
			return nil
		}
		if _, e := tx.Exec(ctx, `UPDATE runs SET head_sha=$2 WHERE id=$1`, r.ID, head); e != nil {
			return e
		}
		return addEvent(ctx, tx, r.ID, r.Epoch, "push", map[string]any{"old": r.HeadSHA, "new": head})
	})
	if err != nil {
		return err
	}
	return inner
}

var ErrUnknown = errors.New("outcome unknown")

// ---- events

type Event struct {
	ID    int64           `json:"id"`
	Epoch int             `json:"epoch"`
	At    time.Time       `json:"at"`
	Kind  string          `json:"kind"`
	Body  json.RawMessage `json:"body"`
}

func (s *Store) Event(ctx context.Context, run string, epoch int, kind string, body map[string]any) error {
	b, _ := json.Marshal(body)
	_, err := s.DB.Exec(ctx, `INSERT INTO events (run_id, epoch, kind, body) VALUES ($1,$2,$3,$4)`, run, epoch, kind, b)
	return err
}

func addEvent(ctx context.Context, tx pgx.Tx, run string, epoch int, kind string, body map[string]any) error {
	b, _ := json.Marshal(body)
	_, err := tx.Exec(ctx, `INSERT INTO events (run_id, epoch, kind, body) VALUES ($1,$2,$3,$4)`, run, epoch, kind, b)
	return err
}

func (s *Store) Events(ctx context.Context, run string) ([]Event, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, epoch, at, kind, body FROM events WHERE run_id=$1 ORDER BY id`, run)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		return e, r.Scan(&e.ID, &e.Epoch, &e.At, &e.Kind, &e.Body)
	})
}

// Denied returns hosts the egress proxy refused for one sandbox in one phase.
func (s *Store) Denied(ctx context.Context, run string, epoch int, sandbox, phase string) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT body->>'host' FROM events
		WHERE run_id=$1 AND epoch=$2 AND kind='egress_denied' AND body->>'sandbox'=$3 AND body->>'phase'=$4 ORDER BY 1`, run, epoch, sandbox, phase)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ---- snapshots

func (s *Store) Snapshot(ctx context.Context, key string) (string, error) {
	var ref string
	err := s.DB.QueryRow(ctx, `SELECT ref FROM snapshots WHERE key=$1`, key).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return ref, err
}

func (s *Store) PutSnapshot(ctx context.Context, key, repo, provider, ref string) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO snapshots (key, repo, provider, ref) VALUES ($1,$2,$3,$4) ON CONFLICT (key) DO NOTHING`, key, repo, provider, ref)
	return err
}

func (s *Store) DropSnapshot(ctx context.Context, key string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM snapshots WHERE key=$1`, key)
	return err
}

// OldSnapshots returns all but the newest keep per repository.
func (s *Store) OldSnapshots(ctx context.Context, provider string, keep int) (map[string]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT key, ref FROM (SELECT key, ref, row_number() OVER (PARTITION BY repo ORDER BY created_at DESC) n
		FROM snapshots WHERE provider=$1) x WHERE n>$2`, provider, keep)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	var k, r string
	_, err = pgx.ForEachRow(rows, []any{&k, &r}, func() error { out[k] = r; return nil })
	return out, err
}

func hash(t string) []byte { h := sha256.Sum256([]byte(t)); return h[:] }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
