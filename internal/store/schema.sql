CREATE TABLE IF NOT EXISTS runs (
  id           text PRIMARY KEY,
  repo         text NOT NULL,
  agent_cmd    text NOT NULL,
  title        text NOT NULL,
  state        text NOT NULL DEFAULT 'queued'
               CHECK (state IN ('queued','running','review','approved','failed','cancelled','attention')),
  epoch        int  NOT NULL DEFAULT 0,
  max_attempts int  NOT NULL DEFAULT 3,
  claimed_by   text,
  claim_until  timestamptz,
  base_sha     text NOT NULL DEFAULT '',
  head_sha     text NOT NULL DEFAULT '',
  approved_sha text NOT NULL DEFAULT '',
  pr_number    int  NOT NULL DEFAULT 0,
  review_until timestamptz,
  code         text NOT NULL DEFAULT '',
  owner        text NOT NULL DEFAULT '',
  detail       jsonb NOT NULL DEFAULT '{}',
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS runs_claim ON runs (created_at) WHERE state IN ('queued','running');

-- One row per sandbox we meant to create, written before the provider is called.
CREATE TABLE IF NOT EXISTS sandboxes (
  name       text PRIMARY KEY,
  run_id     text NOT NULL REFERENCES runs(id),
  epoch      int  NOT NULL,
  role       text NOT NULL CHECK (role IN ('build','agent','verify')),
  provider   text NOT NULL,
  ref        text NOT NULL DEFAULT '',
  state      text NOT NULL DEFAULT 'intent' CHECK (state IN ('intent','live','destroyed')),
  wanted     boolean NOT NULL DEFAULT true,
  token_hash bytea UNIQUE NOT NULL,
  caps       text[] NOT NULL,
  revoked    boolean NOT NULL DEFAULT false,
  endpoint   jsonb,
  phase      text NOT NULL DEFAULT 'provision',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sandboxes_run ON sandboxes (run_id, epoch);
CREATE INDEX IF NOT EXISTS sandboxes_open ON sandboxes (state) WHERE state <> 'destroyed';

CREATE TABLE IF NOT EXISTS events (
  id     bigserial PRIMARY KEY,
  run_id text NOT NULL,
  epoch  int  NOT NULL,
  at     timestamptz NOT NULL DEFAULT now(),
  kind   text NOT NULL,
  body   jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_run ON events (run_id, id);

CREATE TABLE IF NOT EXISTS snapshots (
  key        text PRIMARY KEY,
  repo       text NOT NULL,
  provider   text NOT NULL,
  ref        text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
