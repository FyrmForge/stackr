CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    subject_id  TEXT,
    token       TEXT     NOT NULL UNIQUE,
    expires_at  DATETIME NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
    -- Add your own metadata columns here
);

CREATE INDEX idx_sessions_token ON sessions (token);
CREATE INDEX idx_sessions_subject_id ON sessions (subject_id) WHERE subject_id IS NOT NULL;

CREATE TABLE users (
    id              TEXT PRIMARY KEY,
    email           TEXT     NOT NULL UNIQUE,
    password_hash   TEXT     NOT NULL,
    name            TEXT     NOT NULL DEFAULT '',
    role            TEXT     NOT NULL DEFAULT 'user',
    active          INTEGER  NOT NULL DEFAULT 1,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_users_email ON users (email);
