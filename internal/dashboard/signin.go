package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	msgBadSignIn        = `the body must be {"mode": "oidc" or "jwt" or "", "label", "issuer", "clientID", "clientSecret", "header", "audience", "jwksURL", "emailClaim", "allowedDomains", "allowedEmails"}`
	msgConfigSignIn     = "sign-in is set in the config file; change it there"
	msgSecretRequired   = "the client secret is required for OIDC"
	msgNewIssuerSecret  = "a new issuer needs the client secret entered again: a stored secret is never sent to a new token endpoint"
	msgBadTestBody      = `the body must be {} or {"mode": "oidc" or "jwt", "issuer", "jwksURL"}`
	msgNothingToTest    = "set an issuer (OIDC) or a JWKS URL (JWT) first"
	msgSecretTooLong    = "clientSecret must be at most 16 KiB"
	googleIssuer        = "https://accounts.google.com"
	defaultSignInLabel  = "SSO"
	defaultEmailClaim   = "email"
	maxDiscoveryBytes   = 1 << 20
	maxTestFieldBytes   = 2048
	signInSourceConfig  = "config"
	signInSourceBrowser = "dashboard"
)

// signIn is the sign-in settings in force: the config file's when it sets a
// mode, else the ones saved in the dashboard. The lists are normalised (emails
// and domains lower-cased, ASCII only).
type signIn struct {
	mode, label, issuer, clientID, clientSecret string
	header, audience, jwksURL, emailClaim       string
	domains, emails                             []string
	// fromConfig: the config file sets it, and the dashboard shows it
	// read-only.
	fromConfig bool
}

// oidcReady reports whether OIDC sign-in can start: the mode, an issuer, a
// client ID and a secret.
func (s signIn) oidcReady() bool {
	return s.mode == config.SignInOIDC && s.issuer != "" && s.clientID != "" && s.clientSecret != ""
}

// allowed reports whether a sign-in by email may go on. email is already
// normalised (store.NormalizeEmail) and, for OIDC, proved: the token's
// signature, issuer, audience and expiry were checked and its email is
// verified. claims are the token's claims. added is set when an admin added
// that email first.
//
// It passes when added is set, or the email is exactly on the email list, or
// the part after the last "@" is exactly on the domain list. With Google as
// the issuer a domain rule also needs the token's hd claim to equal that
// domain, so a personal Google account on a company address cannot pass. Both
// lists empty and added unset: nobody.
func (s signIn) allowed(email string, claims map[string]any, added bool) bool {
	if added {
		return true
	}
	email = asciiLower(email)
	if slices.ContainsFunc(s.emails, func(e string) bool { return asciiLower(e) == email }) {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 1 || at == len(email)-1 {
		return false
	}
	domain := email[at+1:]
	if !slices.ContainsFunc(s.domains, func(d string) bool { return asciiLower(d) == domain }) {
		return false
	}
	if s.isGoogle() {
		hd, _ := claims["hd"].(string)
		return hd != "" && asciiLower(hd) == domain
	}
	return true
}

func (s signIn) isGoogle() bool { return strings.TrimSuffix(s.issuer, "/") == googleIssuer }

// asciiLower lower-cases ASCII letters only, as store.NormalizeEmail does.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// normalizedLists makes the allowlist's two lists comparable: domains
// lower-cased, emails through store.NormalizeEmail (an entry that is not an
// address is dropped: it could not match), both without repeats.
func normalizedLists(domains, emails []string) (ds, es []string) {
	for _, d := range domains {
		if d = asciiLower(strings.TrimSpace(d)); d != "" && !slices.Contains(ds, d) {
			ds = append(ds, d)
		}
	}
	for _, e := range emails {
		if n, err := store.NormalizeEmail(strings.TrimSpace(e)); err == nil && !slices.Contains(es, n) {
			es = append(es, n)
		}
	}
	return ds, es
}

// signInSettings reads the sign-in settings in force. A secret stored in the
// dashboard that cannot be opened (PERGENT_SH_SECRET_KEY changed) counts as
// not set, and is logged. In JWT mode an email claim left empty is "email",
// from the config file as from the dashboard.
func (d *Dashboard) signInSettings(ctx context.Context) (signIn, error) {
	s, err := d.readSignIn(ctx)
	if err == nil && s.mode == config.SignInJWT && s.emailClaim == "" {
		s.emailClaim = defaultEmailClaim
	}
	return s, err
}

func (d *Dashboard) readSignIn(ctx context.Context) (signIn, error) {
	if c := d.cfg.Dashboard.SignIn; c.Set() {
		ds, es := normalizedLists(c.AllowedDomains, c.AllowedEmails)
		s := signIn{mode: c.Mode, label: c.Label, issuer: c.Issuer, clientID: c.ClientID,
			header: c.Header, audience: c.Audience, jwksURL: c.JWKSURL, emailClaim: c.EmailClaim,
			domains: ds, emails: es, fromConfig: true}
		if c.ClientSecretEnv != "" {
			s.clientSecret = os.Getenv(c.ClientSecretEnv)
		}
		return s, nil
	}
	if d.Store == nil {
		return signIn{}, nil
	}
	row, err := d.Store.SignInSettings(ctx)
	if err != nil {
		return signIn{}, err
	}
	ds, es := normalizedLists(row.AllowedDomains, row.AllowedEmails)
	s := signIn{mode: row.Mode, label: row.Label, issuer: row.Issuer, clientID: row.ClientID,
		header: row.Header, audience: row.Audience, jwksURL: row.JWKSURL, emailClaim: row.EmailClaim,
		domains: ds, emails: es}
	if len(row.ClientSecretSealed) > 0 {
		plain, err := d.Auth.box.Open(row.ClientSecretSealed)
		if err != nil {
			d.Log.Error("dashboard: the stored sign-in client secret can't be opened with PERGENT_SH_SECRET_KEY; it counts as not set")
		} else {
			s.clientSecret = string(plain)
		}
	}
	return s, nil
}

// EndStaleSSOSessions ends the OIDC and JWT sessions that the sign-in settings
// in force, from the config file or the dashboard, no longer vouch for: a
// session whose sign-in is not the mode in force, or whose issuer is not the
// issuer in force. Changing the settings in the dashboard ends them already; a
// change in the config file, or a rollback, only shows at the next start, so
// the server calls this once before it serves. A local admin's session is never
// touched. It returns how many sessions it ended.
func (d *Dashboard) EndStaleSSOSessions(ctx context.Context) (int, error) {
	s, err := d.signInSettings(ctx)
	if err != nil {
		return 0, err
	}
	return d.Store.DeleteStaleSSOSessions(ctx, s.mode, s.issuer)
}

// ssoView is the sign-in page's button.
type ssoView struct {
	Label string `json:"label"`
}

// ssoButton is the button GET /api/instances offers, nil when OIDC sign-in is
// not set up. A failure to read the settings offers none: the page still
// has the password form.
func (d *Dashboard) ssoButton(ctx context.Context) *ssoView {
	s, err := d.signInSettings(ctx)
	if err != nil {
		d.Log.Error("dashboard: reading the sign-in settings for the sign-in page failed", "error", err.Error())
		return nil
	}
	if !s.oidcReady() {
		return nil
	}
	label := s.label
	if label == "" {
		label = defaultSignInLabel
	}
	return &ssoView{Label: label}
}

// signInView is GET /api/admin/sign-in for the settings in force. The secret
// is never in it, only whether one is set.
func (d *Dashboard) signInView(s signIn) SignInAdminView {
	v := SignInAdminView{Source: signInSourceBrowser, Editable: true, Mode: s.mode, Label: s.label, Issuer: s.issuer,
		ClientID: s.clientID, Header: s.header, Audience: s.audience, JWKSURL: s.jwksURL, EmailClaim: s.emailClaim,
		AllowedDomains: append([]string{}, s.domains...), AllowedEmails: append([]string{}, s.emails...),
		ClientSecret: SecretView{Set: s.clientSecret != ""}, CallbackURL: d.callbackURL()}
	if s.fromConfig {
		v.Source, v.Editable = signInSourceConfig, false
	}
	return v
}

func (d *Dashboard) getSignIn(w http.ResponseWriter, r *http.Request) {
	s, err := d.signInSettings(r.Context())
	if err != nil {
		d.failed(w, r, "reading the sign-in settings", err)
		return
	}
	writeJSON(w, http.StatusOK, d.signInView(s))
}

// putSignInBody is the body of PUT /api/admin/sign-in. ClientSecret is
// absent to keep the stored secret, "" to remove it, or the new secret.
type putSignInBody struct {
	Mode           string   `json:"mode"`
	Label          string   `json:"label"`
	Issuer         string   `json:"issuer"`
	ClientID       string   `json:"clientID"`
	ClientSecret   *string  `json:"clientSecret"`
	Header         string   `json:"header"`
	Audience       string   `json:"audience"`
	JWKSURL        string   `json:"jwksURL"`
	EmailClaim     string   `json:"emailClaim"`
	AllowedDomains []string `json:"allowedDomains"`
	AllowedEmails  []string `json:"allowedEmails"`
}

// putSignIn saves the sign-in settings (409 when the config file sets them).
// Changing the mode or the issuer ends every OIDC and JWT session, in the same
// transaction as the save. Changing the issuer also needs the client secret
// entered again, and a stored one is dropped: it belongs to the old issuer
// and must never be sent to a new token endpoint, not even after a detour
// through another mode. A local admin's session is not touched.
func (d *Dashboard) putSignIn(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	if d.cfg.Dashboard.SignIn.Set() {
		writeError(w, http.StatusConflict, msgConfigSignIn)
		return
	}
	var in putSignInBody
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadSignIn)
		return
	}
	row := store.SignInSettings{
		Mode: strings.TrimSpace(in.Mode), Label: strings.TrimSpace(in.Label), Issuer: strings.TrimSpace(in.Issuer),
		ClientID: strings.TrimSpace(in.ClientID), Header: strings.TrimSpace(in.Header), Audience: strings.TrimSpace(in.Audience),
		JWKSURL: strings.TrimSpace(in.JWKSURL), EmailClaim: strings.TrimSpace(in.EmailClaim),
	}
	if problems := (config.SignIn{Mode: row.Mode, Label: row.Label, Issuer: row.Issuer, ClientID: row.ClientID,
		Header: row.Header, Audience: row.Audience, JWKSURL: row.JWKSURL, EmailClaim: row.EmailClaim,
		AllowedDomains: in.AllowedDomains, AllowedEmails: in.AllowedEmails}).Problems(false); len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	for _, e := range in.AllowedEmails {
		n, err := store.NormalizeEmail(strings.TrimSpace(e))
		if err != nil {
			writeError(w, http.StatusBadRequest, "allowedEmails: "+quoteShort(e)+" is not an email address")
			return
		}
		if !slices.Contains(row.AllowedEmails, n) {
			row.AllowedEmails = append(row.AllowedEmails, n)
		}
	}
	for _, dom := range in.AllowedDomains {
		if dom = asciiLower(strings.TrimSpace(dom)); !slices.Contains(row.AllowedDomains, dom) {
			row.AllowedDomains = append(row.AllowedDomains, dom)
		}
	}
	if row.Mode == config.SignInJWT && row.EmailClaim == "" {
		row.EmailClaim = defaultEmailClaim
	}

	prev, err := d.Store.SignInSettings(ctx)
	if err != nil {
		d.failed(w, r, "reading the sign-in settings", err)
		return
	}
	issuerChanged := prev.Issuer != row.Issuer
	endSessions := prev.Mode != row.Mode || issuerChanged

	hasSecret := len(prev.ClientSecretSealed) > 0 && !issuerChanged
	switch {
	case in.ClientSecret != nil && *in.ClientSecret != "":
		if len(*in.ClientSecret) > maxSecretBytes {
			writeError(w, http.StatusBadRequest, msgSecretTooLong)
			return
		}
		row.ClientSecretSealed = d.Auth.box.Seal([]byte(*in.ClientSecret))
		hasSecret = true
	case in.ClientSecret != nil, issuerChanged:
		// Asked to remove it, or it belonged to another issuer.
		row.ClientSecretSealed = []byte{}
		hasSecret = false
	}
	if row.Mode == config.SignInOIDC && !hasSecret {
		if issuerChanged && len(prev.ClientSecretSealed) > 0 {
			writeError(w, http.StatusBadRequest, msgNewIssuerSecret)
		} else {
			writeError(w, http.StatusBadRequest, msgSecretRequired)
		}
		return
	}
	row.UpdatedAt = d.now()
	if err := d.Store.ChangeSignInSettings(ctx, row, endSessions); err != nil {
		d.Log.Error("dashboard: saving the sign-in settings failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.forgetProvider()

	changed := changedSignIn(prev, row, in.ClientSecret != nil || issuerChanged)
	detail := strings.Join(changed, ", ")
	if endSessions {
		detail += " (SSO sessions ended)"
	}
	d.Log.Info("dashboard: sign-in settings updated", "fields", changed, "by", se.Username)
	d.audit(ctx, se, "sign_in_updated", "", "", detail)

	s, err := d.signInSettings(ctx)
	if err != nil {
		d.failed(w, r, "reading the sign-in settings", err)
		return
	}
	writeJSON(w, http.StatusOK, d.signInView(s))
}

// changedSignIn names the settings that differ, without values: the audit log
// is read by every admin and a secret is never in it.
func changedSignIn(prev, now store.SignInSettings, secretTouched bool) []string {
	var out []string
	for _, f := range []struct {
		name string
		same bool
	}{
		{"mode", prev.Mode == now.Mode}, {"label", prev.Label == now.Label}, {"issuer", prev.Issuer == now.Issuer},
		{"client_id", prev.ClientID == now.ClientID}, {"header", prev.Header == now.Header},
		{"audience", prev.Audience == now.Audience}, {"jwks_url", prev.JWKSURL == now.JWKSURL},
		{"email_claim", prev.EmailClaim == now.EmailClaim},
		{"allowed_domains", slices.Equal(prev.AllowedDomains, now.AllowedDomains)},
		{"allowed_emails", slices.Equal(prev.AllowedEmails, now.AllowedEmails)},
		{"client_secret", !secretTouched},
	} {
		if !f.same {
			out = append(out, f.name)
		}
	}
	if len(out) == 0 {
		out = append(out, "nothing")
	}
	return out
}

// quoteShort is s in quotes, cut at 80 characters.
func quoteShort(s string) string {
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// testSignIn is the test button: it reads the OIDC discovery document or the
// JWKS and answers whether that worked, and nothing else. The body is
// optional; what it leaves out is taken from the settings in force.
// Nothing the other side sent comes back: no status, no error text, no
// document, no address. The answer is {"ok": true|false}. A test that gets as
// far as fetching is written to the audit log with the actor and the mode.
func (d *Dashboard) testSignIn(w http.ResponseWriter, r *http.Request) {
	if mode := d.signInTest(w, r); mode != "" {
		// Who ran it and the mode, nothing the other side said: the button
		// makes the server fetch an address an admin typed.
		d.audit(r.Context(), session(r), "sign_in_tested", "", "", "mode="+mode)
	}
}

// signInTest answers the test button and returns the mode it tested, or ""
// when it refused the request.
func (d *Dashboard) signInTest(w http.ResponseWriter, r *http.Request) string {
	ctx := r.Context()
	var in struct {
		Mode    string `json:"mode"`
		Issuer  string `json:"issuer"`
		JWKSURL string `json:"jwksURL"`
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxTestFieldBytes*4))
	if err != nil {
		writeError(w, http.StatusBadRequest, msgBadTestBody)
		return ""
	}
	if len(bytes.TrimSpace(data)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, msgBadTestBody)
			return ""
		}
	}
	s, err := d.signInSettings(ctx)
	if err != nil {
		d.failed(w, r, "reading the sign-in settings", err)
		return ""
	}
	mode, issuer, jwks := strings.TrimSpace(in.Mode), strings.TrimSpace(in.Issuer), strings.TrimSpace(in.JWKSURL)
	if mode == "" {
		mode = s.mode
	}
	if issuer == "" {
		issuer = s.issuer
	}
	if jwks == "" {
		jwks = s.jwksURL
	}
	if mode != config.SignInOIDC && mode != config.SignInJWT {
		writeError(w, http.StatusBadRequest, msgNothingToTest)
		return ""
	}
	target := issuer
	if mode == config.SignInJWT {
		target = jwks
	}
	if target == "" {
		writeError(w, http.StatusBadRequest, msgNothingToTest)
		return ""
	}
	if problems := (config.SignIn{Issuer: issuer, JWKSURL: jwks}).Problems(false); len(problems) > 0 {
		writeError(w, http.StatusBadRequest, "the issuer and the JWKS URL must be https URLs")
		return ""
	}
	ok := false
	if mode == config.SignInOIDC {
		ok = d.discoveryWorks(ctx, issuer)
	} else {
		ok = d.jwksWorks(ctx, jwks)
	}
	writeJSON(w, http.StatusOK, SignInTestView{OK: ok})
	return mode
}

// fetchSmall GETs rawURL with the identity-provider client and returns the
// body of a 200 answer (at most 1 MiB), else nil. Why it failed is not kept.
func (d *Dashboard) fetchSmall(ctx context.Context, rawURL string) []byte {
	ctx, cancel := context.WithTimeout(ctx, idpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	res, err := d.idp.Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxDiscoveryBytes+1))
	if err != nil || len(body) > maxDiscoveryBytes {
		return nil
	}
	return body
}

// discoveryWorks reads issuer's discovery document and checks what sign-in
// uses: the same issuer, https authorization and token endpoints and a
// https key set.
func (d *Dashboard) discoveryWorks(ctx context.Context, issuer string) bool {
	body := d.fetchSmall(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration")
	var doc struct {
		Issuer  string `json:"issuer"`
		AuthURL string `json:"authorization_endpoint"`
		Token   string `json:"token_endpoint"`
		JWKSURI string `json:"jwks_uri"`
	}
	if body == nil || json.Unmarshal(body, &doc) != nil {
		return false
	}
	return doc.Issuer == issuer && httpsURL(doc.AuthURL) && httpsURL(doc.Token) && httpsURL(doc.JWKSURI)
}

// jwksWorks reads the key set at jwksURL and checks that it holds a key.
func (d *Dashboard) jwksWorks(ctx context.Context, jwksURL string) bool {
	body := d.fetchSmall(ctx, jwksURL)
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	return body != nil && json.Unmarshal(body, &set) == nil && len(set.Keys) > 0
}
