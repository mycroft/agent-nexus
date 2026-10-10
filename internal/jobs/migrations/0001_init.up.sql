CREATE TABLE jobs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL,
    group_key     TEXT NOT NULL,
    dedupe_key    TEXT NOT NULL,
    status        TEXT NOT NULL,
    payload       BLOB NOT NULL,
    checkpoint    BLOB,
    result        BLOB,
    error         TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    api_key_name  TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    started_at    TEXT,
    finished_at   TEXT
);

-- One live job per dedupe key. Failed, skipped and superseded jobs are left
-- out so the same commit can be queued again later.
CREATE UNIQUE INDEX jobs_dedupe_live ON jobs (dedupe_key)
    WHERE status IN ('queued', 'running', 'posting', 'succeeded');

CREATE INDEX jobs_status ON jobs (status, id);
CREATE INDEX jobs_group ON jobs (group_key, status);

CREATE TABLE job_steps (
    job_id            INTEGER NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    seq               INTEGER NOT NULL,
    role              TEXT NOT NULL,
    tool_name         TEXT NOT NULL DEFAULT '',
    content           TEXT NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL,
    PRIMARY KEY (job_id, seq)
);

CREATE INDEX job_steps_created ON job_steps (created_at);
