package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The sign-in modes of Dashboard.SignIn. An empty mode is neither: only
// local admins sign in.
const (
	SignInOIDC = "oidc"
	SignInJWT  = "jwt"
)

// SignIn is how people other than the local admins sign in, as dashboard.sign_in
// sets it. When it is set (Mode is not empty) it wins over what an admin
// saved in the dashboard, which then shows it read-only. The client secret is
// named by an env var, never written here.
//
// OIDC uses Label, Issuer, ClientID and ClientSecretEnv; JWT validation uses
// Header, Issuer, Audience, JWKSURL and EmailClaim; the allowlist is the same
// for both.
type SignIn struct {
	// Mode is "oidc", "jwt" or empty.
	Mode  string `yaml:"mode"`
	Label string `yaml:"label"`
	// Issuer is the OIDC provider's issuer URL (https), or the issuer a JWT
	// must name.
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"client_id"`
	// ClientSecretEnv names the env var holding the OIDC client secret.
	ClientSecretEnv string `yaml:"client_secret_env"`
	// Header is the request header that carries the JWT.
	Header string `yaml:"header"`
	// Audience is the audience a JWT must name.
	Audience string `yaml:"audience"`
	// JWKSURL is where a JWT's signing keys are fetched (https).
	JWKSURL string `yaml:"jwks_url"`
	// EmailClaim is the JWT claim that holds the email; "email" when empty.
	EmailClaim string `yaml:"email_claim"`
	// AllowedDomains and AllowedEmails are the allowlist: an email on either
	// list may sign in; so may one an admin added.
	AllowedDomains []string `yaml:"allowed_domains"`
	AllowedEmails  []string `yaml:"allowed_emails"`
}

// Set reports whether the config file sets the sign-in: its mode is not empty.
func (s SignIn) Set() bool { return s.Mode != "" }

const (
	maxSignInList  = 500
	maxSignInField = 512
)

var (
	signInHeaderRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)
	signInClaimRE  = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)
	signInDomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// Problems lists, one sentence each and without the setting's name in front,
// what is wrong with s. fromFile is true for the config file, where an OIDC
// sign-in names its secret's env var; the dashboard stores the secret
// itself. A field is checked when it is set, whatever the mode; the mode
// decides which are required.
func (s SignIn) Problems(fromFile bool) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	switch s.Mode {
	case "", SignInOIDC, SignInJWT:
	default:
		add(`mode must be "oidc", "jwt" or empty`)
	}
	if n := utf8.RuneCountInString(s.Label); n > 64 || hasControl(s.Label) {
		add("label must be at most 64 characters, with no control characters")
	}
	if s.Issuer != "" {
		if err := checkSignInURL(s.Issuer, true); err != nil {
			add("issuer %v", err)
		}
	}
	if s.JWKSURL != "" {
		if err := checkSignInURL(s.JWKSURL, false); err != nil {
			add("jwks_url %v", err)
		}
	}
	for field, v := range map[string]string{"client_id": s.ClientID, "audience": s.Audience} {
		if len(v) > maxSignInField || hasControl(v) || strings.ContainsAny(v, " \t") {
			add("%s must be at most %d characters, with no spaces or control characters", field, maxSignInField)
		}
	}
	if s.Header != "" && !signInHeaderRE.MatchString(s.Header) {
		add("header must be a header name: letters, digits and - (at most 128)")
	}
	if s.EmailClaim != "" && !signInClaimRE.MatchString(s.EmailClaim) {
		add("email_claim must be a claim name: letters, digits and _ . : / - (at most 128)")
	}
	if len(s.AllowedDomains) > maxSignInList || len(s.AllowedEmails) > maxSignInList {
		add("allowed_domains and allowed_emails take at most %d entries each", maxSignInList)
	} else {
		for _, d := range s.AllowedDomains {
			if !signInDomainRE.MatchString(strings.ToLower(d)) {
				add("allowed_domains: %q is not a domain such as example.com", d)
			}
		}
		for _, e := range s.AllowedEmails {
			if !plausibleEmail(e) {
				add("allowed_emails: %q is not an email address", e)
			}
		}
	}

	switch s.Mode {
	case SignInOIDC:
		if s.Issuer == "" {
			add("issuer is required for oidc")
		}
		if s.ClientID == "" {
			add("client_id is required for oidc")
		}
		if fromFile && s.ClientSecretEnv == "" {
			add("client_secret_env is required for oidc")
		}
	case SignInJWT:
		if s.Issuer == "" {
			add("issuer is required for jwt: a token meant for another app must be refused")
		}
		if s.Audience == "" {
			add("audience is required for jwt: a token meant for another app must be refused")
		}
		if s.JWKSURL == "" {
			add("jwks_url is required for jwt")
		}
		if s.Header == "" {
			add("header is required for jwt")
		}
	case "":
		if fromFile && !s.empty() {
			add("mode is required (oidc or jwt) when other sign_in settings are set")
		}
	}
	if fromFile && s.ClientSecretEnv != "" && !envNameRE.MatchString(s.ClientSecretEnv) {
		add("client_secret_env %q must be an environment variable name (%s)", s.ClientSecretEnv, envNameRE)
	}
	return out
}

// empty reports whether nothing at all is set.
func (s SignIn) empty() bool {
	return s.Mode == "" && s.Label == "" && s.Issuer == "" && s.ClientID == "" && s.ClientSecretEnv == "" &&
		s.Header == "" && s.Audience == "" && s.JWKSURL == "" && s.EmailClaim == "" &&
		len(s.AllowedDomains) == 0 && len(s.AllowedEmails) == 0
}

// checkSignInURL requires an absolute https URL with a host, no credentials
// and no fragment; an issuer may also not have a query. It returns the end of
// the sentence, "must be ...".
func checkSignInURL(raw string, issuer bool) error {
	const what = "must be an https URL with a host, without credentials or a fragment"
	if len(raw) > 2048 {
		return fmt.Errorf("must be at most 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || hasControl(raw) {
		return fmt.Errorf("%s", what)
	}
	if issuer && (u.RawQuery != "" || u.ForceQuery) {
		return fmt.Errorf("%s or a query", what)
	}
	return nil
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) }) >= 0
}

// plausibleEmail is a light check, as the config package can't normalise an
// email as the store does: one @ with something on both sides, no spaces or
// control characters, at most 254 bytes. The dashboard normalises each entry
// before it compares.
func plausibleEmail(e string) bool {
	return len(e) <= 254 && strings.Count(e, "@") == 1 && !strings.HasPrefix(e, "@") && !strings.HasSuffix(e, "@") &&
		!hasControl(e) && !strings.ContainsAny(e, " \t<>,;:\"\\()[]")
}

// DeprecatedWarnings returns one message per config key that is read by
// nothing now and only warned about: the OAuth client of a GitLab or GitHub
// instance, which no longer signs anyone in. A later release refuses them.
func DeprecatedWarnings(c *Config) []string {
	var out []string
	warn := func(prefix, name, idEnv, secretEnv string) {
		if idEnv == "" && secretEnv == "" {
			return
		}
		out = append(out, fmt.Sprintf("%s (%s): oauth_client_id_env and oauth_client_secret_env are ignored: signing in with GitLab or GitHub is gone (see the README, \"Sign-in\"); remove them", prefix, name))
	}
	for i, inst := range c.GitLab {
		warn(fmt.Sprintf("gitlab[%d]", i), inst.Name, inst.OAuthClientIDEnv, inst.OAuthClientSecretEnv)
	}
	for i, inst := range c.GitHub {
		warn(fmt.Sprintf("github[%d]", i), inst.Name, inst.OAuthClientIDEnv, inst.OAuthClientSecretEnv)
	}
	return out
}
