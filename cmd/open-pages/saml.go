package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	saml2 "github.com/russellhaering/gosaml2"
	"github.com/russellhaering/gosaml2/types"
	dsig "github.com/russellhaering/goxmldsig"
	dsigtypes "github.com/russellhaering/goxmldsig/types"

	"github.com/gira0/open-pages/internal/config"
)

// SAML 2.0 web browser SSO: SP-initiated only (HTTP-Redirect AuthnRequest, HTTP-POST
// response). It is only wired up when [saml] enabled = true; see docs/auth.md. XML parsing,
// signature verification (goxmldsig) and the basic assertion checks come from gosaml2; the
// checks that tie a response to this browser's login attempt are done here.

const (
	samlRequestCookie = "saml_request"
	samlCookiePath    = "/v1/auth/saml"
	samlLoginTTL      = 10 * time.Minute
	samlMaxPending    = 10000
	samlMaxUsed       = 50000
	samlHTTPTimeout   = 10 * time.Second
	samlMaxMetadata   = 1 << 20
	samlMaxBody       = 2 << 20 // SAMLResponse form posts; large group lists make responses big
	samlClockSkew     = time.Minute
	samlMetadataTTL   = time.Hour
	samlRetryAfter    = time.Minute // minimum gap between failed metadata refreshes
	samlMaxGroups     = 500
	samlMaxSubjectLen = 512

	nameIDTransient    = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
	nameIDEmailAddress = "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
)

var (
	errSAMLState    = errors.New("unknown or expired login attempt")
	errSAMLInvalid  = errors.New("response failed verification")
	errSAMLUnsigned = errors.New("assertion is not signed")
	errSAMLShape    = errors.New("unexpected assertion structure")
	errSAMLAudience = errors.New("assertion is not for this service")
	errSAMLTime     = errors.New("assertion is outside its validity period")
	errSAMLReplay   = errors.New("assertion was already used")
	errSAMLSubject  = errors.New("assertion has no usable subject")
	errSAMLBusy     = errors.New("too many sign-ins in progress")
)

// samlClient holds the provider's metadata and the login attempts in flight.
type samlClient struct {
	cfg  config.SAMLConfig
	http *http.Client

	signer *saml2.KeyStore // nil unless AuthnRequests are signed
	cert   []byte          // DER certificate published in the SP metadata, if any
	// Cookie flags for the request-tracking cookie; see requestCookie.
	secureCookie bool

	fetchMu   sync.Mutex // guards sp and fetched; held while talking to the provider
	sp        *saml2.SAMLServiceProvider
	fetched   time.Time
	retryFrom time.Time // after a failed refresh, no new attempt before this

	mu      sync.Mutex // guards pending and used
	pending map[string]samlPending
	used    map[string]time.Time // assertion IDs already accepted, until when to remember them
}

// samlPending is the server-side half of a login attempt, keyed by the AuthnRequest ID.
type samlPending struct {
	cookie   string // random value held by the browser that started the attempt
	returnTo string
	expires  time.Time
}

func newSAMLClient(cfg config.SAMLConfig) (*samlClient, error) {
	c := &samlClient{
		cfg: cfg,
		http: &http.Client{
			Timeout: samlHTTPTimeout,
			// Metadata is served directly; a redirect is an error.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		pending: make(map[string]samlPending),
		used:    make(map[string]time.Time),
	}
	// The ACS is reached by a cross-site POST from the identity provider, so the cookie that
	// ties the response to this browser must be SameSite=None, which browsers only accept
	// with Secure. https is taken from the registered ACS URL as well as cookie_secure, so a
	// TLS-terminating proxy with cookie_secure unset still gets a working cookie.
	u, err := url.Parse(cfg.ACSURL)
	if err != nil {
		return nil, fmt.Errorf("saml.acs_url: %w", err)
	}
	c.secureCookie = u.Scheme == "https"

	if cfg.KeyFile != "" {
		pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("saml key_file/cert_file: %w", err)
		}
		signer, ok := pair.PrivateKey.(crypto.Signer)
		if !ok {
			return nil, errors.New("saml key_file: unsupported private key")
		}
		switch signer.Public().(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey:
		default:
			return nil, errors.New("saml key_file: only RSA and ECDSA keys are supported")
		}
		c.signer = &saml2.KeyStore{Signer: signer, Cert: pair.Certificate[0]}
		c.cert = pair.Certificate[0]
	}
	if cfg.IDPMetadataFile != "" {
		// A missing or unreadable file is a configuration error: fail at startup.
		if _, err := c.readMetadata(context.Background()); err != nil {
			return nil, fmt.Errorf("saml idp_metadata_file: %w", err)
		}
	}
	return c, nil
}

// readMetadata returns the raw IdP metadata document from the file or URL.
func (c *samlClient) readMetadata(ctx context.Context) ([]byte, error) {
	if c.cfg.IDPMetadataFile != "" {
		f, err := os.Open(c.cfg.IDPMetadataFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, samlMaxMetadata))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.IDPMetadataURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/samlmetadata+xml, application/xml, text/xml")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", req.URL.Host, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, samlMaxMetadata))
}

// provider returns the service provider configured against the IdP's current metadata,
// fetching it on first use (so the server starts while the IdP is down) and refreshing it
// every samlMetadataTTL to pick up certificate rotation. A failed refresh keeps the old
// metadata.
func (c *samlClient) provider(ctx context.Context) (*saml2.SAMLServiceProvider, error) {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	now := time.Now()
	if c.sp != nil && now.Sub(c.fetched) < samlMetadataTTL {
		return c.sp, nil
	}
	if c.sp == nil && now.Before(c.retryFrom) {
		return nil, errors.New("metadata: recent fetch failed")
	}
	sp, err := c.loadProvider(ctx)
	if err != nil {
		if c.sp != nil {
			c.fetched = now.Add(samlRetryAfter - samlMetadataTTL)
			return c.sp, nil
		}
		c.retryFrom = now.Add(samlRetryAfter)
		return nil, err
	}
	c.sp, c.fetched = sp, now
	return sp, nil
}

func (c *samlClient) loadProvider(ctx context.Context) (*saml2.SAMLServiceProvider, error) {
	raw, err := c.readMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	var md types.EntityDescriptor
	if err := xml.Unmarshal(raw, &md); err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	if md.EntityID == "" || md.IDPSSODescriptor == nil {
		return nil, errors.New("metadata: not an IdP EntityDescriptor")
	}
	var ssoURL string
	for _, s := range md.IDPSSODescriptor.SingleSignOnServices {
		if s.Binding == saml2.BindingHttpRedirect {
			ssoURL = s.Location
			break
		}
	}
	if err := config.CheckURL("metadata SingleSignOnService", ssoURL); err != nil {
		return nil, fmt.Errorf("metadata: no usable HTTP-Redirect single sign-on endpoint: %w", err)
	}
	var roots []*x509.Certificate
	for _, kd := range md.IDPSSODescriptor.KeyDescriptors {
		if kd.Use != "" && kd.Use != "signing" {
			continue
		}
		for _, xc := range kd.KeyInfo.X509Data.X509Certificates {
			der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(xc.Data), ""))
			if err != nil {
				return nil, errors.New("metadata: bad certificate encoding")
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				return nil, errors.New("metadata: bad certificate")
			}
			roots = append(roots, cert)
		}
	}
	if len(roots) == 0 {
		return nil, errors.New("metadata: no signing certificate")
	}

	sp := &saml2.SAMLServiceProvider{
		IdentityProviderSSOURL:      ssoURL,
		IdentityProviderSSOBinding:  saml2.BindingHttpRedirect,
		IdentityProviderIssuer:      md.EntityID,
		ServiceProviderIssuer:       c.cfg.EntityID,
		AudienceURI:                 c.cfg.EntityID,
		AssertionConsumerServiceURL: c.cfg.ACSURL,
		IDPCertificateStore:         &dsig.MemoryX509CertificateStore{Roots: roots},
		// Groups are optional, and the checks on the assertion are made in accept.
		AllowMissingAttributes: true,
	}
	if c.signer != nil {
		// Only the signing key is set, never the encryption key: with no decryption key an
		// EncryptedAssertion is refused rather than read.
		if err := sp.SetSPSigningKeyStore(c.signer); err != nil {
			return nil, err
		}
		sp.SignAuthnRequests = true
		sp.SignAuthnRequestsAlgorithm = dsig.RSASHA256SignatureMethod
		if _, ok := c.signer.Signer.Public().(*ecdsa.PublicKey); ok {
			sp.SignAuthnRequestsAlgorithm = dsig.ECDSASHA256SignatureMethod
		}
	}
	return sp, nil
}

// begin records a login attempt for the AuthnRequest id and returns the cookie value that
// binds it to the browser. It returns false when too many attempts are pending.
func (c *samlClient) begin(id, returnTo string) (cookie string, ok bool) {
	cookie, err := generateToken()
	if err != nil {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) >= samlMaxPending { // expired attempts are only swept when the map is full
		for k, p := range c.pending {
			if now.After(p.expires) {
				delete(c.pending, k)
			}
		}
		if len(c.pending) >= samlMaxPending {
			return "", false
		}
	}
	c.pending[id] = samlPending{cookie: cookie, returnTo: returnTo, expires: now.Add(samlLoginTTL)}
	return cookie, true
}

// take removes and returns the attempt for the AuthnRequest id if it is still valid and
// belongs to the browser holding cookie. An attempt presented with the wrong cookie stays.
func (c *samlClient) take(id, cookie string) (samlPending, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[id]
	if !ok {
		return samlPending{}, false
	}
	if time.Now().After(p.expires) {
		delete(c.pending, id)
		return samlPending{}, false
	}
	if subtle.ConstantTimeCompare([]byte(p.cookie), []byte(cookie)) != 1 {
		return samlPending{}, false
	}
	delete(c.pending, id)
	return p, true
}

// markUsed records an accepted assertion ID until the given time and reports whether it was
// new. An assertion can only be accepted while its login attempt is pending, so remembering
// the ID for as long as that attempt could last covers every possible replay.
func (c *samlClient) markUsed(id string, until time.Time) error {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if exp, seen := c.used[id]; seen && now.Before(exp) {
		return errSAMLReplay
	}
	if len(c.used) >= samlMaxUsed { // expired entries are only swept when the map is full
		for k, exp := range c.used {
			if now.After(exp) {
				delete(c.used, k)
			}
		}
		if len(c.used) >= samlMaxUsed {
			return errSAMLBusy
		}
	}
	c.used[id] = until
	return nil
}

// setSAMLCookie sets or clears the cookie that ties a response to the browser that
// started the login. Unlike oidc_state it must be sent on the identity provider's cross-site
// POST to the ACS, which a SameSite=Lax cookie is not; so it is SameSite=None (with Secure,
// which browsers require for None). Without https the cookie falls back to Lax, which only
// works when the provider and this service are on the same site (for example both below
// corp.example) or for local development. It is HttpOnly, scoped to /v1/auth/saml and lives
// 10 minutes. Being sent cross-site does not expose anything new: it is a random value that
// is useless without a response from the provider that names the matching request ID.
func (s *Server) setSAMLCookie(w http.ResponseWriter, value string, maxAge int) {
	secure := s.saml.secureCookie || s.cfg.CookieSecure
	sameSite := http.SameSiteLaxMode
	if secure {
		sameSite = http.SameSiteNoneMode
	}
	s.setCookieAttrs(w, samlRequestCookie, samlCookiePath, value, maxAge, secure, sameSite)
}

// handleSAMLLogin sends the browser to the IdP with a (possibly signed) AuthnRequest. An
// optional return_to (a local path) is where the ACS sends it afterwards.
func (s *Server) handleSAMLLogin(w http.ResponseWriter, r *http.Request) {
	returnTo := r.URL.Query().Get("return_to")
	if returnTo != "" && !safeReturnTo(returnTo) {
		writeError(w, http.StatusBadRequest, "return_to must be a path on this server")
		return
	}
	sp, err := s.saml.provider(r.Context())
	if err != nil {
		oidcUnavailable(w, "saml metadata")
		return
	}
	doc, err := sp.BuildAuthRequestDocumentNoSig() // the redirect binding signs the query instead
	if err != nil || doc.Root() == nil {
		internalError(w, "saml authn request", errors.New("build failed"))
		return
	}
	reqID := doc.Root().SelectAttrValue("ID", "")
	redirect, err := sp.BuildAuthURLRedirect("", doc)
	if reqID == "" || err != nil {
		internalError(w, "saml authn request", errors.New("build failed"))
		return
	}
	cookie, ok := s.saml.begin(reqID, returnTo)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "too many sign-in attempts, try again later")
		return
	}
	s.setSAMLCookie(w, cookie, int(samlLoginTTL.Seconds()))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, redirect, http.StatusFound)
}

// handleSAMLACS is the assertion consumer service: it verifies the IdP's response, finds or
// creates the user, maps groups and starts a normal session.
func (s *Server) handleSAMLACS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Without the cookie this response cannot belong to a login this browser started. This
	// also refuses IdP-initiated and unsolicited responses before any XML is parsed.
	cookie, err := r.Cookie(samlRequestCookie)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusBadRequest, errSAMLState.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, samlMaxBody)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form")
		return
	}
	encoded := strings.NewReplacer("\r", "", "\n", "").Replace(r.PostForm.Get("SAMLResponse"))
	if encoded == "" {
		writeError(w, http.StatusBadRequest, "SAMLResponse is required")
		return
	}
	sp, err := s.saml.provider(r.Context())
	if err != nil {
		oidcUnavailable(w, "saml metadata")
		return
	}
	res, err := s.saml.accept(sp, encoded, cookie.Value)
	if err != nil {
		if errors.Is(err, errSAMLState) {
			writeError(w, http.StatusBadRequest, errSAMLState.Error())
			return
		}
		if errors.Is(err, errSAMLBusy) {
			writeError(w, http.StatusServiceUnavailable, "too many sign-ins in progress, try again later")
			return
		}
		// Only a fixed reason is logged, never anything taken from the response.
		slog.Warn("saml login rejected", "reason", samlRejectionReason(err), "request_id", requestIDOf(w))
		writeError(w, http.StatusUnauthorized, "the identity provider's response was not accepted")
		return
	}
	// The attempt is used up; the cookie has done its job.
	s.setSAMLCookie(w, "", -1)

	id := res.identity
	if len(id.groups) > samlMaxGroups {
		id.groups = id.groups[:samlMaxGroups]
	}
	var uid int64
	if err = checkEmail(id.email, s.saml.cfg.AllowedEmailDomain); err == nil {
		if uid, err = s.federatedUser(r.Context(), id); err == nil {
			if err = s.syncFederatedGroups(r.Context(), uid, id.groups); err == nil {
				err = s.startSession(w, r, uid)
			}
		}
	}
	switch {
	case errors.Is(err, errNoEmail), errors.Is(err, errEmailDomain):
		slog.Warn("saml login refused", "reason", rejectionReason(err), "request_id", requestIDOf(w))
		writeError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, errEmailTaken):
		slog.Warn("saml login refused", "reason", rejectionReason(err), "request_id", requestIDOf(w))
		writeError(w, http.StatusConflict, err.Error()+"; it is not linked automatically, ask an administrator")
		return
	case err != nil:
		internalError(w, "saml login", err)
		return
	}
	if res.returnTo != "" {
		http.Redirect(w, r, res.returnTo, http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "successful login"})
}

// samlRejectionReason maps an error to a fixed string that is safe to log.
func samlRejectionReason(err error) string {
	for _, known := range []error{errSAMLInvalid, errSAMLUnsigned, errSAMLShape, errSAMLAudience,
		errSAMLTime, errSAMLReplay, errSAMLSubject} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "other"
}

type samlResult struct {
	identity federatedIdentity
	returnTo string
}

// accept verifies an encoded SAMLResponse and returns the identity it asserts. Order matters:
// everything below the signature check is done on the signature-verified element only.
func (c *samlClient) accept(sp *saml2.SAMLServiceProvider, encoded, cookie string) (samlResult, error) {
	// gosaml2 verifies the XML signature against the IdP metadata's certificates (a signed
	// Response, or each Assertion signed; an unsigned Assertion is only accepted inside a
	// signed Response), then the issuer, status, destination, Recipient and
	// SubjectConfirmationData expiry. Encrypted assertions fail: no decryption key is set.
	resp, err := sp.ValidateEncodedResponse(encoded)
	if err != nil {
		return samlResult{}, errSAMLInvalid
	}
	// Exactly one assertion, so there is no ambiguity about which one was checked.
	if len(resp.Assertions) != 1 || len(resp.EncryptedAssertions) != 0 {
		return samlResult{}, errSAMLShape
	}
	a := &resp.Assertions[0]
	if !resp.SignatureValidated && !a.SignatureValidated {
		return samlResult{}, errSAMLUnsigned
	}
	if a.ID == "" || len(a.ID) > 256 || a.Subject == nil || a.Subject.NameID == nil || a.Subject.SubjectConfirmation == nil ||
		a.Subject.SubjectConfirmation.SubjectConfirmationData == nil {
		return samlResult{}, errSAMLShape
	}

	warn, err := sp.VerifyAssertionConditions(a)
	if err != nil {
		return samlResult{}, errSAMLShape
	}
	// An assertion must name this service as an audience; one with no restriction at all is
	// not enough.
	if warn.NotInAudience || len(a.Conditions.AudienceRestrictions) == 0 {
		return samlResult{}, errSAMLAudience
	}
	// The library's time warning has no clock skew; apply our own.
	if !validNow(a.Conditions, time.Now()) {
		return samlResult{}, errSAMLTime
	}

	// The request this assertion answers comes from the signed assertion itself. A
	// Response-level InResponseTo only counts when the Response signature was verified.
	reqID := a.Subject.SubjectConfirmation.SubjectConfirmationData.InResponseTo
	if reqID == "" || (resp.SignatureValidated && resp.InResponseTo != "" && resp.InResponseTo != reqID) {
		return samlResult{}, errSAMLState
	}
	pending, ok := c.take(reqID, cookie)
	if !ok {
		return samlResult{}, errSAMLState
	}
	if err := c.markUsed(a.ID, pending.expires.Add(samlClockSkew)); err != nil {
		return samlResult{}, err
	}

	id := federatedIdentity{issuer: sp.IdentityProviderIssuer}
	nameID := a.Subject.NameID
	if attr := c.cfg.NameIDAttribute; attr != "" {
		// The configured attribute is the stable identifier, so the NameID format does not matter.
		id.subject = firstAttribute(a, attr)
	} else {
		if nameID.Format == nameIDTransient {
			return samlResult{}, errSAMLSubject // a different value at every login
		}
		id.subject = strings.TrimSpace(nameID.Value)
	}
	if id.subject == "" || len(id.subject) > samlMaxSubjectLen {
		return samlResult{}, errSAMLSubject
	}
	id.email = firstAttribute(a, c.cfg.EmailAttribute)
	if id.email == "" && nameID.Format == nameIDEmailAddress {
		id.email = strings.TrimSpace(nameID.Value)
	}
	if c.cfg.GroupsAttribute != "" {
		id.groups = attributeValues(a, c.cfg.GroupsAttribute, samlMaxGroups)
	}
	return samlResult{identity: id, returnTo: pending.returnTo}, nil
}

// validNow checks NotBefore and NotOnOrAfter with a little clock skew.
func validNow(cond *types.Conditions, now time.Time) bool {
	nb, err1 := time.Parse(time.RFC3339, cond.NotBefore)
	na, err2 := time.Parse(time.RFC3339, cond.NotOnOrAfter)
	if err1 != nil || err2 != nil {
		return false
	}
	return !now.Add(samlClockSkew).Before(nb) && now.Add(-samlClockSkew).Before(na)
}

// attributeValues returns the non-empty values of the attributes called name (matched on
// Name or FriendlyName), at most limit of them.
func attributeValues(a *types.Assertion, name string, limit int) []string {
	var out []string
	if a.AttributeStatement == nil {
		return nil
	}
	for _, at := range a.AttributeStatement.Attributes {
		if at.Name != name && at.FriendlyName != name {
			continue
		}
		for _, v := range at.Values {
			if s := strings.TrimSpace(v.Value); s != "" && len(out) < limit {
				out = append(out, s)
			}
		}
	}
	return out
}

func firstAttribute(a *types.Assertion, name string) string {
	if v := attributeValues(a, name, 1); len(v) > 0 {
		return v[0]
	}
	return ""
}

// handleSAMLMetadata serves this service's SP metadata, for registering it at the IdP. It
// needs no login and does not depend on the IdP being reachable.
func (s *Server) handleSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	c := s.saml
	sp := &types.SPSSODescriptor{
		AuthnRequestsSigned:        c.signer != nil,
		WantAssertionsSigned:       true,
		ProtocolSupportEnumeration: saml2.SAMLProtocolNamespace,
		AssertionConsumerServices: []types.IndexedEndpoint{{
			Binding: saml2.BindingHttpPost, Location: c.cfg.ACSURL, Index: 1,
		}},
	}
	if c.cert != nil {
		kd := types.KeyDescriptor{Use: "signing"}
		kd.KeyInfo.X509Data.X509Certificates = []dsigtypes.X509Certificate{{Data: base64.StdEncoding.EncodeToString(c.cert)}}
		sp.KeyDescriptors = append(sp.KeyDescriptors, kd)
	}
	md := types.EntityDescriptor{
		ValidUntil:      time.Now().UTC().Add(7 * 24 * time.Hour),
		EntityID:        c.cfg.EntityID,
		SPSSODescriptor: sp,
	}
	body, err := xml.MarshalIndent(md, "", "  ")
	if err != nil {
		internalError(w, "saml metadata", err)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, _ = w.Write(append([]byte(xml.Header), body...))
}
