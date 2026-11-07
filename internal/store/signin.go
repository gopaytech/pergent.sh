package store

import (
	"context"
	"fmt"
	"time"
)

// The sign-in modes. An empty Mode is neither: only local admins sign in.
const (
	SignInModeOIDC = "oidc"
	SignInModeJWT  = "jwt"
)

// SignInSettings are the sign-in settings set in the browser: one row at
// most. Mode is "", SignInModeOIDC or SignInModeJWT. Label, Issuer,
// ClientID and ClientSecretSealed are OIDC's; Header, Audience, JWKSURL and
// EmailClaim are JWT's; Issuer is also JWT's. AllowedDomains and
// AllowedEmails are the allowlist, as the caller normalised them.
//
// ClientSecretSealed is the OIDC client secret, sealed by the caller with
// PERGENT_SH_SECRET_KEY; the store only keeps the bytes.
type SignInSettings struct {
	Mode, Label, Issuer, ClientID string
	Header, Audience, JWKSURL     string
	EmailClaim                    string
	AllowedDomains, AllowedEmails []string
	ClientSecretSealed            []byte
	UpdatedAt                     time.Time
}

// SignInSettings returns the sign-in settings. When none were ever put it
// is the zero value (Mode ""), not an error. A list with no entries is nil.
func (s *Store) SignInSettings(ctx context.Context) (SignInSettings, error) {
	var r SignInSettings
	rows, err := s.pool.Query(ctx, `SELECT mode, label, issuer, client_id, header, audience, jwks_url, email_claim,
			allowed_domains, allowed_emails, client_secret_sealed, updated_at
		FROM sign_in_settings WHERE id = 1`)
	if err != nil {
		return SignInSettings{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return SignInSettings{}, rows.Err()
	}
	if err := rows.Scan(&r.Mode, &r.Label, &r.Issuer, &r.ClientID, &r.Header, &r.Audience, &r.JWKSURL, &r.EmailClaim,
		&r.AllowedDomains, &r.AllowedEmails, &r.ClientSecretSealed, &r.UpdatedAt); err != nil {
		return SignInSettings{}, err
	}
	if len(r.AllowedDomains) == 0 {
		r.AllowedDomains = nil
	}
	if len(r.AllowedEmails) == 0 {
		r.AllowedEmails = nil
	}
	return r, rows.Err()
}

// PutSignInSettings stores the settings, replacing the earlier ones (UpdatedAt
// is the time of the call when the field is zero). A nil ClientSecretSealed keeps the
// secret already stored; an empty, non-nil one removes it. Putting settings
// does not end any session: DeleteSSOSessions does.
func (s *Store) PutSignInSettings(ctx context.Context, in SignInSettings) error {
	if in.Mode != "" && in.Mode != SignInModeOIDC && in.Mode != SignInModeJWT {
		return fmt.Errorf("store: unknown sign-in mode %q", in.Mode)
	}
	domains, emails := in.AllowedDomains, in.AllowedEmails
	if domains == nil {
		domains = []string{}
	}
	if emails == nil {
		emails = []string{}
	}
	// secretGiven tells "keep" (nil) from "remove" (empty) from "replace".
	secretGiven := in.ClientSecretSealed != nil
	secret := in.ClientSecretSealed
	at := in.UpdatedAt
	if at.IsZero() {
		at = time.Now()
	}
	if len(secret) == 0 {
		secret = nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sign_in_settings (id, mode, label, issuer, client_id, client_secret_sealed,
			header, audience, jwks_url, email_claim, allowed_domains, allowed_emails, updated_at)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $13)
		ON CONFLICT (id) DO UPDATE SET mode = EXCLUDED.mode, label = EXCLUDED.label, issuer = EXCLUDED.issuer,
			client_id = EXCLUDED.client_id, header = EXCLUDED.header, audience = EXCLUDED.audience,
			jwks_url = EXCLUDED.jwks_url, email_claim = EXCLUDED.email_claim,
			allowed_domains = EXCLUDED.allowed_domains, allowed_emails = EXCLUDED.allowed_emails,
			updated_at = EXCLUDED.updated_at,
			client_secret_sealed = CASE WHEN $12 THEN EXCLUDED.client_secret_sealed ELSE sign_in_settings.client_secret_sealed END`,
		in.Mode, in.Label, in.Issuer, in.ClientID, secret, in.Header, in.Audience, in.JWKSURL, in.EmailClaim,
		domains, emails, secretGiven, at)
	return err
}
