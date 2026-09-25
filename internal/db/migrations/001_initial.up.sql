CREATE TABLE sessions (
    id          TEXT     PRIMARY KEY,
    subject_id  TEXT,
    token       TEXT     NOT NULL UNIQUE,
    expires_at  DATETIME NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_sessions_token ON sessions (token);
CREATE INDEX idx_sessions_subject_id ON sessions (subject_id) WHERE subject_id IS NOT NULL;

-- role 'admin' is the stackr admin; org ownership lives in org_members.
CREATE TABLE users (
    id              TEXT     PRIMARY KEY,
    email           TEXT     NOT NULL UNIQUE,
    password_hash   TEXT     NOT NULL,
    name            TEXT     NOT NULL,
    role            TEXT     NOT NULL CHECK (role IN ('admin', 'user')),
    active          INTEGER  NOT NULL,
    avatar_path     TEXT     NOT NULL,
    theme           TEXT     NOT NULL,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL
);
