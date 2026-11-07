-- Sign-in and access: SSO accounts identified by their email, roles on
-- accounts and on team memberships, how a session began, and the sign-in
-- settings. This migration only adds. Nothing is dropped or renamed, so the
-- previous release still runs on the migrated database: platform accounts,
-- user_access, teams.role, the session platform columns and the OAuth
-- secrets stay in place, unused. The next release drops them.

-- Every sign-in ends: each person signs in again, and a session now records
-- how it began.
DELETE FROM sessions;

-- accounts.kind gains 'sso'. An SSO account is the one row of a lower-cased
-- email. It has no password and no platform identity; its username is its
-- email, and the platform columns stay NULL because the previous release
-- reads them with COALESCE. The checks that tie 'password' and 'platform'
-- rows to their columns are unchanged.
ALTER TABLE accounts DROP CONSTRAINT accounts_kind_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_kind_check CHECK (kind IN ('password', 'platform', 'sso'));
ALTER TABLE accounts
    ADD COLUMN email text,
    -- A password account is always an admin; the store reads it as one
    -- whatever this column says, so a row the previous release makes after a
    -- rollback (default 'user') is still an admin.
    ADD COLUMN role  text    NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
    -- added is set when an admin typed the email before its first sign-in,
    -- and stays set afterwards.
    ADD COLUMN added boolean NOT NULL DEFAULT false;
UPDATE accounts SET role = 'admin' WHERE kind = 'password';
ALTER TABLE accounts ADD CONSTRAINT accounts_sso_columns CHECK (
    kind <> 'sso' OR (email IS NOT NULL AND password_hash IS NULL AND platform IS NULL
                      AND instance IS NULL AND platform_user_id IS NULL));
ALTER TABLE accounts ADD CONSTRAINT accounts_email_only_sso CHECK (kind = 'sso' OR email IS NULL);
-- The store keeps emails lower-cased (ASCII only), and looks them up by the
-- exact value. The second index refuses two addresses that a locale's
-- lower() would fold into one, so such a pair can never share an account.
CREATE UNIQUE INDEX accounts_email ON accounts (email);
CREATE UNIQUE INDEX accounts_email_lower ON accounts (lower(email));

-- A member's role on the team. A team's own role is no longer read.
ALTER TABLE team_members
    ADD COLUMN role text NOT NULL DEFAULT 'viewer' CHECK (role IN ('viewer', 'maintainer'));

-- How a session began: with a password, or through OIDC or a JWT from a
-- proxy, and for an SSO session the issuer that vouched for it.
ALTER TABLE sessions
    ADD COLUMN sign_in text NOT NULL DEFAULT 'password' CHECK (sign_in IN ('password', 'oidc', 'jwt')),
    ADD COLUMN issuer  text NOT NULL DEFAULT '';

-- The sign-in settings set in the browser: one row at most. mode '' is
-- neither OIDC nor JWT. The OIDC client secret is sealed with
-- PERGENT_SH_SECRET_KEY.
CREATE TABLE sign_in_settings (
    id                   integer     PRIMARY KEY CHECK (id = 1),
    mode                 text        NOT NULL DEFAULT '' CHECK (mode IN ('', 'oidc', 'jwt')),
    label                text        NOT NULL DEFAULT '',
    issuer               text        NOT NULL DEFAULT '',
    client_id            text        NOT NULL DEFAULT '',
    client_secret_sealed bytea,
    header               text        NOT NULL DEFAULT '',
    audience             text        NOT NULL DEFAULT '',
    jwks_url             text        NOT NULL DEFAULT '',
    email_claim          text        NOT NULL DEFAULT '',
    allowed_domains      text[]      NOT NULL DEFAULT '{}',
    allowed_emails       text[]      NOT NULL DEFAULT '{}',
    updated_at           timestamptz NOT NULL
);
