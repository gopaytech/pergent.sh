-- Accounts, teams and the audit log. Admin groups read from the platforms
-- give way to password admin accounts and in-app teams, and every session
-- belongs to an account.

-- Nothing is released yet, so every sign-in is dropped (and its user_access
-- with it): each user signs in again and gets an account.
DELETE FROM sessions;

-- A sign-in identity. A password account (an operator) signs in with a
-- username and password; a platform account is a GitLab or GitHub user that
-- signed in with OAuth on one instance.
CREATE TABLE accounts (
    id                   bigserial   PRIMARY KEY,
    kind                 text        NOT NULL CHECK (kind IN ('password', 'platform')),
    username             text        NOT NULL,
    platform             text,
    instance             text,
    platform_user_id     bigint,
    password_hash        text,
    must_change_password boolean     NOT NULL DEFAULT false,
    failed_attempts      integer     NOT NULL DEFAULT 0,
    locked_until         timestamptz,
    created_at           timestamptz NOT NULL,
    last_sign_in_at      timestamptz,
    CHECK (kind <> 'password' OR password_hash IS NOT NULL),
    CHECK (kind <> 'platform' OR (platform IS NOT NULL AND instance IS NOT NULL AND platform_user_id IS NOT NULL))
);
CREATE UNIQUE INDEX accounts_password_username ON accounts (lower(username)) WHERE kind = 'password';
CREATE UNIQUE INDEX accounts_platform_user ON accounts (instance, platform_user_id) WHERE kind = 'platform';

CREATE TABLE teams (
    id         bigserial   PRIMARY KEY,
    name       text        NOT NULL,
    role       text        NOT NULL CHECK (role IN ('admin', 'maintainer', 'viewer')),
    created_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX teams_name ON teams (lower(name));

CREATE TABLE team_members (
    team_id    bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    account_id bigint NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, account_id)
);
CREATE INDEX team_members_account ON team_members (account_id);

-- The repositories a team covers; project '' is every repository of the
-- instance.
CREATE TABLE team_repos (
    team_id  bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    instance text   NOT NULL,
    project  text   NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX team_repos_key ON team_repos (team_id, instance, lower(project));

-- Sessions belong to an account. A password session has no platform, no
-- instance, no platform user and no OAuth token. access_scope is what the
-- last access check allowed the session.
ALTER TABLE sessions
    ADD COLUMN account_id   bigint NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    ADD COLUMN access_scope text   NOT NULL DEFAULT '',
    ALTER COLUMN token_sealed DROP NOT NULL,
    ALTER COLUMN platform DROP NOT NULL,
    ALTER COLUMN instance DROP NOT NULL,
    ALTER COLUMN user_id DROP NOT NULL;
CREATE INDEX sessions_account_idx ON sessions (account_id);

-- What admins did. actor_id has no foreign key, so an entry outlives the
-- account that made it.
CREATE TABLE audit_log (
    id       bigserial   PRIMARY KEY,
    at       timestamptz NOT NULL,
    actor_id bigint,
    actor    text        NOT NULL,
    action   text        NOT NULL,
    instance text        NOT NULL DEFAULT '',
    project  text        NOT NULL DEFAULT '',
    detail   text        NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_at_idx ON audit_log (at);
