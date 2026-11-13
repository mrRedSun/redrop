CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    totp_secret   TEXT NOT NULL DEFAULT '',
    totp_enabled  INTEGER NOT NULL DEFAULT 0,
    role          TEXT NOT NULL DEFAULT 'user',
    quota_bytes   INTEGER NOT NULL,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);

CREATE TABLE identities (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,
    subject    TEXT NOT NULL,
    email      TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (provider, subject)
);

CREATE TABLE sessions (
    hash         TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    method       TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    hash         TEXT NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    expires_at   INTEGER
);
CREATE INDEX tokens_user ON tokens(user_id);

-- One-time links: invites, password resets, first-run setup.
CREATE TABLE onetime (
    id          TEXT PRIMARY KEY,
    hash        TEXT NOT NULL UNIQUE,
    purpose     TEXT NOT NULL, -- invite | reset | setup
    user_id     TEXT REFERENCES users(id) ON DELETE CASCADE,
    created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
    role        TEXT NOT NULL DEFAULT 'user',
    quota_bytes INTEGER NOT NULL DEFAULT 0,
    clear_totp  INTEGER NOT NULL DEFAULT 0,
    note        TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,
    used_by     TEXT REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE artifacts (
    id              TEXT PRIMARY KEY,
    slug            TEXT NOT NULL UNIQUE,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_id        TEXT REFERENCES tokens(id) ON DELETE SET NULL,
    token_name      TEXT NOT NULL DEFAULT '',
    title           TEXT NOT NULL DEFAULT '',
    current_version INTEGER NOT NULL DEFAULT 0,
    password_hash   TEXT NOT NULL DEFAULT '',
    max_views       INTEGER NOT NULL DEFAULT 0,
    views           INTEGER NOT NULL DEFAULT 0,
    previews        INTEGER NOT NULL DEFAULT 0,
    access_gen      INTEGER NOT NULL DEFAULT 1,
    revoked         INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    expires_at      INTEGER,
    exhausted_at    INTEGER,
    last_viewed_at  INTEGER
);
CREATE INDEX artifacts_user ON artifacts(user_id, created_at);

CREATE TABLE versions (
    artifact_id TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    number      INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    size        INTEGER NOT NULL,
    files       INTEGER NOT NULL,
    entry       TEXT NOT NULL DEFAULT '',
    spa         INTEGER NOT NULL DEFAULT 0,
    note        TEXT NOT NULL DEFAULT '',
    token_name  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, number)
);

CREATE TABLE files (
    artifact_id  TEXT NOT NULL,
    version      INTEGER NOT NULL,
    path         TEXT NOT NULL,
    blob         TEXT NOT NULL,
    size         INTEGER NOT NULL,
    content_type TEXT NOT NULL,
    PRIMARY KEY (artifact_id, version, path),
    FOREIGN KEY (artifact_id, version) REFERENCES versions(artifact_id, number) ON DELETE CASCADE
);
CREATE INDEX files_blob ON files(blob);

CREATE TABLE views (
    artifact_id TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    at          INTEGER NOT NULL,
    outcome     TEXT NOT NULL,
    country     TEXT NOT NULL DEFAULT '',
    client      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX views_artifact ON views(artifact_id, at);

CREATE TABLE uploads (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    size       INTEGER NOT NULL,
    received   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
