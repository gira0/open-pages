package server

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"

	"github.com/gira0/open-pages/internal/config"
)

const (
	testSPEntityID = "https://pages.example/saml/sp"
	samlTimeFormat = "2006-01-02T15:04:05Z"
)

// samlAttr is one attribute of the assertion.
type samlAttr struct {
	name   string
	values []string
}

// samlSettings is what the fake IdP puts into the next response. The zero value of every
// field means "behave correctly".
type samlSettings struct {
	nameID, nameIDFormat string
	noNameID             bool   // a Subject with only the bearer SubjectConfirmation
	email                string // sent as the "email" attribute unless noEmail
	noEmail              bool
	groups               []string
	attrs                []samlAttr

	sign        string // "assertion" (default), "response", "both" or "none"
	rogueKey    bool   // sign with a key the metadata does not publish
	audience    string // replaces the SP entity ID; "-" drops the AudienceRestriction
	recipient   string // replaces the ACS URL in SubjectConfirmationData
	destination string // replaces the ACS URL in Response/@Destination
	issuer      string // replaces the IdP entity ID
	status      string // replaces the success status code

	notBefore, notOnOrAfter time.Duration // offsets from now; zero means -1 minute and +5 minutes
	inResponseTo            string        // replaces the request ID; "-" drops it
	assertionID             string        // fixed assertion ID
	extraEvil               bool          // an unsigned assertion for evil@corp.example placed before the real one
	tamper                  func(doc string) string
}

// fakeSAMLIdP is an in-process SAML identity provider: metadata and an SSO endpoint whose
// "login" is played by respond.
type fakeSAMLIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	rogue *rsa.PrivateKey
	cert  []byte

	mu       sync.Mutex
	set      samlSettings
	metaCode int // status served for the metadata, 200 by default
}

type sentRequest struct{ id, acs, issuer, destination, issueInstant string }

func newTestCert(t *testing.T, key *rsa.PrivateKey, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

var (
	samlKeysOnce       sync.Once
	samlKey, samlRogue *rsa.PrivateKey
)

// sharedSAMLKeys returns two RSA keys generated once per test run (key generation is slow).
func sharedSAMLKeys(t *testing.T) (key, rogue *rsa.PrivateKey) {
	t.Helper()
	samlKeysOnce.Do(func() {
		var err1, err2 error
		samlKey, err1 = rsa.GenerateKey(rand.Reader, 2048)
		samlRogue, err2 = rsa.GenerateKey(rand.Reader, 2048)
		if err1 != nil || err2 != nil {
			panic("generate test keys")
		}
	})
	return samlKey, samlRogue
}

func newFakeSAMLIdP(t *testing.T) *fakeSAMLIdP {
	t.Helper()
	p := &fakeSAMLIdP{metaCode: http.StatusOK, set: samlSettings{nameID: "persistent-1", email: "ann@corp.example"}}
	p.key, p.rogue = sharedSAMLKeys(t)
	p.cert = newTestCert(t, p.key, time.Now().Add(24*time.Hour))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metadata", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		code := p.metaCode
		p.mu.Unlock()
		if code != http.StatusOK {
			http.Error(w, "nope", code)
			return
		}
		_, _ = w.Write([]byte(p.metadata()))
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeSAMLIdP) entityID() string { return p.srv.URL + "/idp" }

func (p *fakeSAMLIdP) metadata() string {
	p.mu.Lock()
	cert := base64.StdEncoding.EncodeToString(p.cert)
	p.mu.Unlock()
	return `<?xml version="1.0"?>
<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" entityID="` + p.entityID() + `">
 <md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
  <md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + cert + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>
  <md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="` + p.srv.URL + `/sso-post"/>
  <md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="` + p.srv.URL + `/sso"/>
 </md:IDPSSODescriptor>
</md:EntityDescriptor>`
}

func (p *fakeSAMLIdP) update(f func(s *samlSettings)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(&p.set)
}

// rotate makes the IdP publish and sign with a new key.
func (p *fakeSAMLIdP) rotate(t *testing.T) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.key, p.cert = key, newTestCert(t, key, time.Now().Add(24*time.Hour))
	p.mu.Unlock()
}

// readRequest decodes the AuthnRequest of a redirect to the SSO endpoint.
func (p *fakeSAMLIdP) readRequest(t *testing.T, location string) (sentRequest, url.Values) {
	t.Helper()
	if !strings.HasPrefix(location, p.srv.URL+"/sso?") {
		t.Fatalf("login redirected to %q, want the IdP's redirect-binding endpoint", location)
	}
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	raw, err := base64.StdEncoding.DecodeString(q.Get("SAMLRequest"))
	if err != nil {
		t.Fatal(err)
	}
	xmlBytes, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(xmlBytes); err != nil {
		t.Fatal(err)
	}
	root := doc.Root()
	req := sentRequest{
		id:           root.SelectAttrValue("ID", ""),
		acs:          root.SelectAttrValue("AssertionConsumerServiceURL", ""),
		destination:  root.SelectAttrValue("Destination", ""),
		issueInstant: root.SelectAttrValue("IssueInstant", ""),
	}
	if iss := root.SelectElement("Issuer"); iss != nil {
		req.issuer = iss.Text()
	}
	if req.id == "" || req.issuer != testSPEntityID || req.destination != p.srv.URL+"/sso" {
		t.Fatalf("unexpected AuthnRequest: %+v", req)
	}
	return req, q
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (p *fakeSAMLIdP) signer(rogue bool) *dsig.SigningContext {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := p.key
	if rogue {
		key = p.rogue
	}
	ctx := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore(tls.Certificate{Certificate: [][]byte{p.cert}, PrivateKey: key}))
	// Exclusive canonicalization, as IdPs use: the assertion's digest must not depend on the
	// namespaces declared around it.
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	return ctx
}

// sign signs el and inserts the signature right after its Issuer.
func (p *fakeSAMLIdP) sign(t *testing.T, el *etree.Element, rogue bool) *etree.Element {
	t.Helper()
	sig, err := p.signer(rogue).ConstructSignature(el, true)
	if err != nil {
		t.Fatal(err)
	}
	out := el.Copy()
	children := []etree.Token{out.Child[0], sig}
	out.Child = append(children, out.Child[1:]...)
	return out
}

func parseElement(t *testing.T, s string) *etree.Element {
	t.Helper()
	doc := etree.NewDocument()
	if err := doc.ReadFromString(s); err != nil {
		t.Fatal(err)
	}
	return doc.Root()
}

// respond plays the user's login at the IdP: it answers the AuthnRequest behind location
// with the base64 SAMLResponse the browser would post to the ACS.
func (p *fakeSAMLIdP) respond(t *testing.T, location string) string {
	t.Helper()
	req, _ := p.readRequest(t, location)
	return p.build(t, req, req.id)
}

func (p *fakeSAMLIdP) build(t *testing.T, req sentRequest, requestID string) string {
	t.Helper()
	p.mu.Lock()
	s := p.set
	p.mu.Unlock()
	now := time.Now().UTC()
	nb, na := s.notBefore, s.notOnOrAfter
	if nb == 0 {
		nb = -time.Minute
	}
	if na == 0 {
		na = 5 * time.Minute
	}
	issuer := p.entityID()
	if s.issuer != "" {
		issuer = s.issuer
	}
	audience := testSPEntityID
	if s.audience != "" {
		audience = s.audience
	}
	recipient, dest := req.acs, req.acs
	if s.recipient != "" {
		recipient = s.recipient
	}
	if s.destination != "" {
		dest = s.destination
	}
	irt := requestID
	if s.inResponseTo != "" {
		irt = s.inResponseTo
	}
	irtAttr := ` InResponseTo="` + esc(irt) + `"`
	if irt == "-" {
		irtAttr = ""
	}
	status := "urn:oasis:names:tc:SAML:2.0:status:Success"
	if s.status != "" {
		status = s.status
	}
	assertionID := s.assertionID
	if assertionID == "" {
		assertionID = fmt.Sprintf("_a%d", time.Now().UnixNano())
	}
	format := s.nameIDFormat
	if format == "" {
		format = "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent"
	}
	attrs := append([]samlAttr(nil), s.attrs...)
	if !s.noEmail {
		attrs = append(attrs, samlAttr{"email", []string{s.email}})
	}
	if s.groups != nil {
		attrs = append(attrs, samlAttr{"groups", s.groups})
	}
	var attrXML strings.Builder
	for _, a := range attrs {
		attrXML.WriteString(`<saml:Attribute Name="` + esc(a.name) + `">`)
		for _, v := range a.values {
			attrXML.WriteString(`<saml:AttributeValue>` + esc(v) + `</saml:AttributeValue>`)
		}
		attrXML.WriteString(`</saml:Attribute>`)
	}
	audXML := `<saml:AudienceRestriction><saml:Audience>` + esc(audience) + `</saml:Audience></saml:AudienceRestriction>`
	if s.audience == "-" {
		audXML = ""
	}
	nameIDXML := func(nameID string) string {
		if s.noNameID {
			return ""
		}
		return `<saml:NameID Format="` + format + `">` + esc(nameID) + `</saml:NameID>`
	}
	assertion := func(id, nameID string) string {
		return `<saml:Assertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="` + id + `" Version="2.0" IssueInstant="` + now.Format(samlTimeFormat) + `">` +
			`<saml:Issuer>` + esc(issuer) + `</saml:Issuer>` +
			`<saml:Subject>` + nameIDXML(nameID) +
			`<saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData` + irtAttr +
			` NotOnOrAfter="` + now.Add(na).Format(samlTimeFormat) + `" Recipient="` + esc(recipient) + `"/></saml:SubjectConfirmation></saml:Subject>` +
			`<saml:Conditions NotBefore="` + now.Add(nb).Format(samlTimeFormat) + `" NotOnOrAfter="` + now.Add(na).Format(samlTimeFormat) + `">` + audXML + `</saml:Conditions>` +
			`<saml:AttributeStatement>` + attrXML.String() + `</saml:AttributeStatement>` +
			`<saml:AuthnStatement AuthnInstant="` + now.Format(samlTimeFormat) + `"><saml:AuthnContext><saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:Password</saml:AuthnContextClassRef></saml:AuthnContext></saml:AuthnStatement>` +
			`</saml:Assertion>`
	}
	signAssertion := s.sign == "" || s.sign == "assertion" || s.sign == "both"
	signResponse := s.sign == "response" || s.sign == "both"
	a := parseElement(t, assertion(assertionID, s.nameID))
	if signAssertion {
		a = p.sign(t, a, s.rogueKey)
	}
	resp := parseElement(t, `<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="_r`+
		fmt.Sprint(time.Now().UnixNano())+`" Version="2.0" IssueInstant="`+now.Format(samlTimeFormat)+`" Destination="`+esc(dest)+`"`+irtAttr+`>`+
		`<saml:Issuer>`+esc(issuer)+`</saml:Issuer>`+
		`<samlp:Status><samlp:StatusCode Value="`+status+`"/></samlp:Status></samlp:Response>`)
	if s.extraEvil {
		resp.AddChild(parseElement(t, assertion("_evil", "evil-subject")))
	}
	resp.AddChild(a)
	if signResponse {
		resp = p.sign(t, resp, s.rogueKey)
	}
	doc := etree.NewDocument()
	doc.SetRoot(resp)
	out, err := doc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	if s.tamper != nil {
		out = s.tamper(out)
	}
	return base64.StdEncoding.EncodeToString([]byte(out))
}

// samlEnv is a running open-pages with SAML pointed at a fake IdP.
type samlEnv struct {
	ts  *httptest.Server
	s   *Server
	idp *fakeSAMLIdP
	acs string
}

// newSAMLTestServer starts open-pages with SAML enabled. mutate adjusts the config.
func newSAMLTestServer(t *testing.T, idp *fakeSAMLIdP, mutate func(*config.Config)) *samlEnv {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil) // the ACS URL has to be known before the server is built
	acs := "http://" + ts.Listener.Addr().String() + "/v1/auth/saml/acs"
	cfg := config.Default()
	cfg.DataPath, cfg.TmpPath = t.TempDir(), t.TempDir()
	cfg.AppHost, cfg.ContentDomain = "127.0.0.1", testContentDomain
	cfg.SAML = config.SAMLConfig{
		Enabled: true, IDPMetadataURL: idp.srv.URL + "/metadata", EntityID: testSPEntityID, ACSURL: acs,
		EmailAttribute: "email", GroupsAttribute: "groups",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		ts.Close()
		t.Fatal(err)
	}
	ts.Config.Handler = s.Handler()
	ts.Start()
	t.Cleanup(func() { ts.Close(); s.db.Close() })
	return &samlEnv{ts: ts, s: s, idp: idp, acs: cfg.SAML.ACSURL}
}

func (e *samlEnv) postACS(t *testing.T, c *http.Client, response string) (int, http.Header) {
	t.Helper()
	resp, err := c.PostForm(e.ts.URL+"/v1/auth/saml/acs", url.Values{"SAMLResponse": {response}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header
}

// start opens the login route and returns the IdP redirect.
func (e *samlEnv) start(t *testing.T, c *http.Client, query string) string {
	t.Helper()
	code, _, h := oidcGet(t, c, e.ts.URL+"/v1/auth/saml/login"+query)
	expectStatus(t, code, http.StatusFound)
	return h.Get("Location")
}

// signIn runs the whole flow for the IdP's current settings and returns the ACS status.
func (e *samlEnv) signIn(t *testing.T, c *http.Client) int {
	t.Helper()
	code, _ := e.postACS(t, c, e.idp.respond(t, e.start(t, c, "")))
	return code
}

func TestSAMLHappyPath(t *testing.T) {
	for _, mode := range []string{"url", "file"} {
		t.Run(mode, func(t *testing.T) {
			idp := newFakeSAMLIdP(t)
			ts := newSAMLTestServer(t, idp, func(c *config.Config) {
				if mode == "file" {
					f := filepath.Join(t.TempDir(), "idp.xml")
					if err := os.WriteFile(f, []byte(idp.metadata()), 0o600); err != nil {
						t.Fatal(err)
					}
					c.SAML.IDPMetadataURL, c.SAML.IDPMetadataFile = "", f
				}
			})
			s := ts.s
			admin := loggedInClient(t, ts.ts)
			eng := newGroupVia(t, ts.ts, admin, "Eng")
			newGroupVia(t, ts.ts, admin, "ops")
			idp.update(func(s *samlSettings) { s.groups = []string{"eng", "no-such-group"} })

			c := browser(t)
			expectStatus(t, ts.signIn(t, c), http.StatusOK)
			if got := sessionEmail(t, ts.ts, c); got != "ann@corp.example" {
				t.Fatalf("session email = %q", got)
			}
			var issuer, subject, password string
			if err := s.db.QueryRow("SELECT oidc_issuer, oidc_subject, password FROM user WHERE oidc_subject IS NOT NULL").
				Scan(&issuer, &subject, &password); err != nil {
				t.Fatal(err)
			}
			if issuer != idp.entityID() || subject != "persistent-1" || password != "" {
				t.Fatalf("user row = %q %q %q", issuer, subject, password)
			}
			var uid int64
			if err := s.db.QueryRow("SELECT userid FROM user WHERE oidc_subject = 'persistent-1'").Scan(&uid); err != nil {
				t.Fatal(err)
			}
			gs, err := s.userGroups(t.Context(), uid)
			if err != nil || len(gs) != 1 || gs[0].ID != eng {
				t.Fatalf("groups = %v (%v), want only Eng", gs, err)
			}
			// SAML accounts have no password either.
			creds := []byte(`{"email":"ann@corp.example","password":"anything goes"}`)
			expectStatus(t, post(t, newClient(t), ts.ts.URL+"/v1/user/login", "application/json", creds), http.StatusUnauthorized)

			// The same subject with a new email is the same user, and groups follow the assertion.
			idp.update(func(s *samlSettings) { s.groups = []string{"OPS"}; s.email = "ann.new@corp.example" })
			c2 := browser(t)
			expectStatus(t, ts.signIn(t, c2), http.StatusOK)
			if n := countUsers(t, s); n != 2 { // the admin and ann
				t.Fatalf("users = %d, want 2", n)
			}
			if got := sessionEmail(t, ts.ts, c2); got != "ann@corp.example" {
				t.Fatalf("email = %q, want the one stored at first login", got)
			}
			gs, err = s.userGroups(t.Context(), uid)
			if err != nil || len(gs) != 1 || gs[0].ID == eng {
				t.Fatalf("groups after second login = %v (%v), want only ops", gs, err)
			}
		})
	}
}

func TestSAMLSignedResponseVariants(t *testing.T) {
	cases := map[string]struct {
		sign string
		ok   bool
	}{
		"assertion signed":            {"assertion", true},
		"response signed":             {"response", true},
		"both signed":                 {"both", true},
		"nothing signed":              {"none", false},
		"signed by an unknown key":    {"assertion", false},
		"response signed by unknown":  {"response", false},
		"both signed by unknown keys": {"both", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			idp := newFakeSAMLIdP(t)
			idp.update(func(s *samlSettings) { s.sign = tc.sign; s.rogueKey = strings.Contains(name, "unknown") })
			ts := newSAMLTestServer(t, idp, nil)
			c := browser(t)
			code := ts.signIn(t, c)
			if tc.ok {
				expectStatus(t, code, http.StatusOK)
				return
			}
			expectStatus(t, code, http.StatusUnauthorized)
			if sessionEmail(t, ts.ts, c) != "" || countUsers(t, ts.s) != 0 {
				t.Fatal("a rejected response produced a session or a user")
			}
		})
	}
}

func TestSAMLRejectsBadAssertions(t *testing.T) {
	cases := map[string]func(s *samlSettings){
		"wrong audience":     func(s *samlSettings) { s.audience = "https://other.example/sp" },
		"no audience":        func(s *samlSettings) { s.audience = "-" },
		"expired":            func(s *samlSettings) { s.notBefore, s.notOnOrAfter = -2*time.Hour, -time.Hour },
		"not yet valid":      func(s *samlSettings) { s.notBefore, s.notOnOrAfter = time.Hour, 2*time.Hour },
		"wrong recipient":    func(s *samlSettings) { s.recipient = "https://evil.example/v1/auth/saml/acs" },
		"wrong destination":  func(s *samlSettings) { s.destination = "https://evil.example/v1/auth/saml/acs" },
		"wrong issuer":       func(s *samlSettings) { s.issuer = "https://evil.example/idp" },
		"IdP refused":        func(s *samlSettings) { s.status = "urn:oasis:names:tc:SAML:2.0:status:Responder" },
		"transient name id":  func(s *samlSettings) { s.nameIDFormat = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient" },
		"empty name id":      func(s *samlSettings) { s.nameID = "  " },
		"garbage response":   func(s *samlSettings) { s.tamper = func(string) string { return "<not-saml/>" } },
		"truncated response": func(s *samlSettings) { s.tamper = func(d string) string { return d[:len(d)/2] } },
		"tampered name id": func(s *samlSettings) {
			s.tamper = func(d string) string { return strings.Replace(d, "persistent-1", "admin-1", 1) }
		},
		"tampered audience": func(s *samlSettings) {
			s.tamper = func(d string) string { return strings.Replace(d, "pages.example", "pages.evil", 1) }
		},
		"unsigned wrapper":    func(s *samlSettings) { s.extraEvil = true },
		"unsigned wrapper #2": func(s *samlSettings) { s.extraEvil = true; s.sign = "both" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			idp := newFakeSAMLIdP(t)
			idp.update(mutate)
			if name == "tampered name id" {
				idp.update(func(s *samlSettings) { s.sign = "both" })
			}
			ts := newSAMLTestServer(t, idp, nil)
			c := browser(t)
			if code := ts.signIn(t, c); code != http.StatusUnauthorized {
				t.Fatalf("ACS status = %d, want 401", code)
			}
			if sessionEmail(t, ts.ts, c) != "" || countUsers(t, ts.s) != 0 {
				t.Fatal("a rejected response produced a session or a user")
			}
		})
	}
}

func TestSAMLClockSkew(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	// An IdP clock 30 seconds ahead is tolerated; five minutes ahead is not.
	idp.update(func(s *samlSettings) { s.notBefore = 30 * time.Second })
	ts := newSAMLTestServer(t, idp, nil)
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
	idp.update(func(s *samlSettings) { s.notBefore = 5 * time.Minute; s.notOnOrAfter = 10 * time.Minute })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusUnauthorized)
}

func TestSAMLClockSkewOnExpiry(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	// An IdP clock 30 seconds behind: the assertion and its SubjectConfirmationData expired
	// 30 seconds ago by our clock, which the skew tolerates.
	idp.update(func(s *samlSettings) { s.notBefore, s.notOnOrAfter = -5*time.Minute, -30*time.Second })
	ts := newSAMLTestServer(t, idp, nil)
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
	// Two minutes behind is too much.
	idp.update(func(s *samlSettings) { s.notBefore, s.notOnOrAfter = -5*time.Minute, -2*time.Minute })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusUnauthorized)
}

func TestSAMLAuthnRequestIssueInstant(t *testing.T) {
	// The skewed clock is only for validating responses; requests carry the real time.
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, nil)
	req, _ := idp.readRequest(t, ts.start(t, browser(t), ""))
	at, err := time.Parse(time.RFC3339, req.issueInstant)
	if err != nil {
		t.Fatalf("IssueInstant %q: %v", req.issueInstant, err)
	}
	if d := time.Since(at); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("IssueInstant is %v off the current time", d)
	}
}

func TestSAMLUnsolicitedResponses(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, nil)

	t.Run("no cookie", func(t *testing.T) {
		// Another browser, without the cookie, delivers a valid response (login CSRF).
		loc := ts.start(t, browser(t), "")
		code, _ := ts.postACS(t, browser(t), idp.respond(t, loc))
		expectStatus(t, code, http.StatusBadRequest)
	})
	t.Run("IdP initiated", func(t *testing.T) {
		c := browser(t)
		loc := ts.start(t, c, "")
		req, _ := idp.readRequest(t, loc)
		// A response that answers no request, delivered by a browser that has the cookie.
		idp.update(func(s *samlSettings) { s.inResponseTo = "-" })
		code, _ := ts.postACS(t, c, idp.build(t, req, req.id))
		expectStatus(t, code, http.StatusBadRequest)
		idp.update(func(s *samlSettings) { s.inResponseTo = "" })
	})
	t.Run("unknown request id", func(t *testing.T) {
		c := browser(t)
		loc := ts.start(t, c, "")
		req, _ := idp.readRequest(t, loc)
		code, _ := ts.postACS(t, c, idp.build(t, req, "_never-issued"))
		expectStatus(t, code, http.StatusBadRequest)
	})
	t.Run("another browsers request", func(t *testing.T) {
		// The attacker has a cookie of their own but delivers a response to the victim's request.
		victimLoc := ts.start(t, browser(t), "")
		attacker := browser(t)
		ts.start(t, attacker, "")
		code, _ := ts.postACS(t, attacker, idp.respond(t, victimLoc))
		expectStatus(t, code, http.StatusBadRequest)
	})
	t.Run("missing or bad form", func(t *testing.T) {
		c := browser(t)
		ts.start(t, c, "")
		code, _ := ts.postACS(t, c, "")
		expectStatus(t, code, http.StatusBadRequest)
		code, _ = ts.postACS(t, c, "%%%not-base64")
		expectStatus(t, code, http.StatusUnauthorized)
		expectStatus(t, get(t, c, ts.ts.URL+"/v1/auth/saml/acs"), http.StatusMethodNotAllowed) // POST only
	})
	if countUsers(t, ts.s) != 0 {
		t.Fatal("an unsolicited response created a user")
	}
}

func TestSAMLReplay(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, nil)

	t.Run("same response twice", func(t *testing.T) {
		c := browser(t)
		resp := idp.respond(t, ts.start(t, c, ""))
		code, _ := ts.postACS(t, c, resp)
		expectStatus(t, code, http.StatusOK)
		code, _ = ts.postACS(t, c, resp)
		expectStatus(t, code, http.StatusBadRequest)
	})
	t.Run("assertion id reused for a new request", func(t *testing.T) {
		idp.update(func(s *samlSettings) { s.assertionID = "_fixed-assertion" })
		expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
		expectStatus(t, ts.signIn(t, browser(t)), http.StatusUnauthorized)
	})
}

func TestSAMLNoEmailTakeover(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, nil)
	userClient(t, ts.ts, "ann@corp.example") // a local account with the same email
	idp.update(func(s *samlSettings) { s.email = "Ann@Corp.example" })
	c := browser(t)
	expectStatus(t, ts.signIn(t, c), http.StatusConflict)
	if sessionEmail(t, ts.ts, c) != "" || countUsers(t, ts.s) != 1 {
		t.Fatal("a SAML login took over or duplicated a local account")
	}
}

func TestSAMLEmailRules(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, func(c *config.Config) { c.SAML.AllowedEmailDomain = "corp.example" })
	idp.update(func(s *samlSettings) { s.email = "mallory@other.example" })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusForbidden)
	idp.update(func(s *samlSettings) { s.noEmail = true })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusForbidden)
	// An email-format NameID stands in for a missing attribute.
	idp.update(func(s *samlSettings) {
		s.nameID, s.nameIDFormat = "bob@corp.example", "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
	})
	c := browser(t)
	expectStatus(t, ts.signIn(t, c), http.StatusOK)
	if got := sessionEmail(t, ts.ts, c); got != "bob@corp.example" {
		t.Fatalf("email = %q", got)
	}
}

func TestSAMLNameIDAttribute(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, func(c *config.Config) { c.SAML.NameIDAttribute = "employeeNumber" })
	idp.update(func(s *samlSettings) { s.attrs = []samlAttr{{"employeeNumber", []string{"E-42"}}} })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
	var subject string
	if err := ts.s.db.QueryRow("SELECT oidc_subject FROM user").Scan(&subject); err != nil || subject != "E-42" {
		t.Fatalf("subject = %q (%v), want the attribute value", subject, err)
	}
	// The attribute is required once configured, even though a NameID is present.
	idp.update(func(s *samlSettings) { s.attrs = nil })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusUnauthorized)
}

func TestSAMLNoNameID(t *testing.T) {
	// Without nameid_attribute the NameID is the subject, so an assertion without one fails.
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, nil)
	idp.update(func(s *samlSettings) { s.noNameID = true })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusUnauthorized)

	// With nameid_attribute a Subject that only carries the bearer confirmation is fine.
	idp2 := newFakeSAMLIdP(t)
	ts2 := newSAMLTestServer(t, idp2, func(c *config.Config) { c.SAML.NameIDAttribute = "employeeNumber" })
	idp2.update(func(s *samlSettings) {
		s.noNameID = true
		s.attrs = []samlAttr{{"employeeNumber", []string{"E-7"}}}
	})
	expectStatus(t, ts2.signIn(t, browser(t)), http.StatusOK)
	var subject string
	if err := ts2.s.db.QueryRow("SELECT oidc_subject FROM user").Scan(&subject); err != nil || subject != "E-7" {
		t.Fatalf("subject = %q (%v), want the attribute value", subject, err)
	}
	// No email attribute and no NameID to fall back on: refused for the missing email, not a panic.
	idp2.update(func(s *samlSettings) {
		s.noEmail = true
		s.attrs = []samlAttr{{"employeeNumber", []string{"E-8"}}}
	})
	expectStatus(t, ts2.signIn(t, browser(t)), http.StatusForbidden)
}

func TestSAMLNameIDAttributeWithTransientNameID(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, func(c *config.Config) { c.SAML.NameIDAttribute = "employeeNumber" })
	idp.update(func(s *samlSettings) {
		s.nameIDFormat = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
		s.attrs = []samlAttr{{"employeeNumber", []string{"E-42"}}}
	})
	// A transient NameID is fine when the subject comes from the attribute: both sign-ins land
	// on the same account.
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
	var n int
	var subject string
	if err := ts.s.db.QueryRow("SELECT COUNT(*), MAX(oidc_subject) FROM user").Scan(&n, &subject); err != nil || n != 1 || subject != "E-42" {
		t.Fatalf("users = %d, subject = %q (%v), want one user keyed on the attribute", n, subject, err)
	}
}

func TestSAMLMappingOffKeepsManagedMemberships(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, func(c *config.Config) { c.SAML.GroupsAttribute = "" })
	eng := newGroupVia(t, ts.ts, loggedInClient(t, ts.ts), "eng")
	idp.update(func(s *samlSettings) { s.groups = []string{"eng"} })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
	var uid int64
	if err := ts.s.db.QueryRow("SELECT userid FROM user WHERE oidc_subject IS NOT NULL").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	// A membership an earlier, mapping-on configuration managed.
	if _, err := ts.s.db.Exec("INSERT INTO user_group (uid, gid, oidc) VALUES (?, ?, 1)", uid, eng); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
	var n int
	if err := ts.s.db.QueryRow("SELECT COUNT(*) FROM user_group WHERE uid = ? AND oidc = 1", uid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("provider-managed memberships = %d (%v), want 1 kept with mapping off", n, err)
	}
}

func TestSAMLGroupsAttributeMultiValued(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, func(c *config.Config) { c.SAML.GroupsAttribute = "memberOf" })
	admin := loggedInClient(t, ts.ts)
	newGroupVia(t, ts.ts, admin, "a")
	newGroupVia(t, ts.ts, admin, "b")
	idp.update(func(s *samlSettings) { s.attrs = []samlAttr{{"memberOf", []string{"a", "b", ""}}} })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
	var n int
	if err := ts.s.db.QueryRow("SELECT COUNT(*) FROM user_group WHERE oidc = 1").Scan(&n); err != nil || n != 2 {
		t.Fatalf("provider-managed memberships = %d (%v), want 2", n, err)
	}
}

func TestSAMLReturnTo(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, nil)
	for _, bad := range []string{"https://evil.example/", "//evil.example", "/\\evil.example", "relative"} {
		code, _, _ := oidcGet(t, browser(t), ts.ts.URL+"/v1/auth/saml/login?return_to="+url.QueryEscape(bad))
		expectStatus(t, code, http.StatusBadRequest)
	}
	c := browser(t)
	loc := ts.start(t, c, "?return_to="+url.QueryEscape("/index"))
	code, h := ts.postACS(t, c, idp.respond(t, loc))
	expectStatus(t, code, http.StatusSeeOther)
	if h.Get("Location") != "/index" {
		t.Fatalf("redirected to %q", h.Get("Location"))
	}
}

func TestSAMLSignedAuthnRequest(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := newTestCert(t, key, time.Now().Add(time.Hour))
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	keyFile, certFile := filepath.Join(dir, "sp.key"), filepath.Join(dir, "sp.crt")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, func(c *config.Config) { c.SAML.KeyFile, c.SAML.CertFile = keyFile, certFile })
	c := browser(t)
	loc := ts.start(t, c, "")
	_, q := idp.readRequest(t, loc)
	// The redirect binding signs "SAMLRequest=..&SigAlg=.." (RelayState is not used).
	signed := "SAMLRequest=" + url.QueryEscape(q.Get("SAMLRequest")) + "&SigAlg=" + url.QueryEscape(q.Get("SigAlg"))
	sig, err := base64.StdEncoding.DecodeString(q.Get("Signature"))
	if err != nil || q.Get("SigAlg") != "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256" {
		t.Fatalf("missing or odd signature parameters: %v (%v)", q, err)
	}
	sum := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("AuthnRequest signature does not verify: %v", err)
	}
	code, _ := ts.postACS(t, c, idp.respond(t, loc))
	expectStatus(t, code, http.StatusOK)

	// The metadata advertises the signing certificate.
	_, body, _ := oidcGet(t, browser(t), ts.ts.URL+"/v1/auth/saml/metadata")
	if !strings.Contains(body, `AuthnRequestsSigned="true"`) || !strings.Contains(body, base64.StdEncoding.EncodeToString(der)) {
		t.Fatalf("metadata lacks the signing key:\n%s", body)
	}
}

func TestSAMLBadKeyFiles(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	base := config.SAMLConfig{Enabled: true, IDPMetadataURL: idp.srv.URL + "/metadata", EntityID: "sp", ACSURL: "https://p.example/acs"}
	missing := base
	missing.KeyFile, missing.CertFile = filepath.Join(t.TempDir(), "no.key"), filepath.Join(t.TempDir(), "no.crt")
	if _, err := newSAMLClient(missing); err == nil {
		t.Error("a missing key file must fail startup")
	}
	noFile := base
	noFile.IDPMetadataURL, noFile.IDPMetadataFile = "", filepath.Join(t.TempDir(), "idp.xml")
	if _, err := newSAMLClient(noFile); err == nil {
		t.Error("a missing metadata file must fail startup")
	}
}

func TestSAMLRejectsECDSAKey(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyFile, certFile := filepath.Join(dir, "sp.key"), filepath.Join(dir, "sp.crt")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = newSAMLClient(config.SAMLConfig{Enabled: true, IDPMetadataURL: "https://idp.example/m", EntityID: "sp",
		ACSURL: "https://p.example/acs", KeyFile: keyFile, CertFile: certFile})
	if err == nil || !strings.Contains(err.Error(), "only RSA keys are supported") {
		t.Fatalf("err = %v, want the RSA-only error", err)
	}
}

func TestSAMLMetadataFetchIgnoresCancelledRequest(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	c, err := newSAMLClient(config.SAMLConfig{Enabled: true, IDPMetadataURL: idp.srv.URL + "/metadata", EntityID: "sp",
		ACSURL: "https://p.example/acs"})
	if err != nil {
		t.Fatal(err)
	}
	// A login request whose client has gone away still fetches the metadata, so it cannot
	// put sign-in into the failed-fetch back-off for everyone else.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.provider(ctx); err != nil {
		t.Fatalf("provider with a cancelled request context: %v", err)
	}
	if !c.retryFrom.IsZero() || c.fetched.IsZero() {
		t.Fatalf("retryFrom = %v, fetched = %v; want a successful fetch and no back-off", c.retryFrom, c.fetched)
	}
	if _, err := c.provider(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSAMLMetadataEndpoint(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	idp.srv.Close() // the SP metadata does not need the IdP
	ts := newSAMLTestServer(t, idp, nil)
	code, body, h := oidcGet(t, browser(t), ts.ts.URL+"/v1/auth/saml/metadata")
	expectStatus(t, code, http.StatusOK)
	if !strings.Contains(h.Get("Content-Type"), "xml") {
		t.Errorf("Content-Type = %q", h.Get("Content-Type"))
	}
	var md struct {
		EntityID string `xml:"entityID,attr"`
		SP       struct {
			Signed  string `xml:"AuthnRequestsSigned,attr"`
			Want    string `xml:"WantAssertionsSigned,attr"`
			Keys    []any  `xml:"KeyDescriptor"`
			Service []struct {
				Binding  string `xml:"Binding,attr"`
				Location string `xml:"Location,attr"`
			} `xml:"AssertionConsumerService"`
		} `xml:"SPSSODescriptor"`
	}
	if err := xml.Unmarshal([]byte(body), &md); err != nil {
		t.Fatalf("metadata is not XML: %v\n%s", err, body)
	}
	if md.EntityID != testSPEntityID || md.SP.Signed != "false" || md.SP.Want != "true" || len(md.SP.Keys) != 0 ||
		len(md.SP.Service) != 1 || md.SP.Service[0].Location != ts.acs ||
		md.SP.Service[0].Binding != "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" {
		t.Fatalf("unexpected metadata: %+v", md)
	}
}

func TestSAMLRequestCookieFlags(t *testing.T) {
	cases := map[string]struct {
		acs, sameSite string
		secure        bool
	}{
		"https acs":  {"https://pages.example/v1/auth/saml/acs", "None", true},
		"http acs":   {"http://127.0.0.1:1/v1/auth/saml/acs", "Lax", false},
		"http+flag":  {"http://127.0.0.1:1/v1/auth/saml/acs", "None", true},
		"https+flag": {"https://pages.example/v1/auth/saml/acs", "None", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			idp := newFakeSAMLIdP(t)
			ts := newSAMLTestServer(t, idp, func(c *config.Config) {
				c.SAML.ACSURL = tc.acs
				c.CookieSecure = strings.HasSuffix(name, "+flag")
			})
			resp, err := browser(t).Get(ts.ts.URL + "/v1/auth/saml/login")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			wantName, wantPath := samlRequestCookie, "/v1/auth/saml"
			if tc.secure {
				wantName, wantPath = "__Host-"+samlRequestCookie, "/"
			}
			var found bool
			for _, ck := range resp.Cookies() {
				if ck.Name != wantName {
					continue
				}
				found = true
				ss := map[http.SameSite]string{http.SameSiteNoneMode: "None", http.SameSiteLaxMode: "Lax"}[ck.SameSite]
				if !ck.HttpOnly || ck.Secure != tc.secure || ss != tc.sameSite || ck.Path != wantPath || ck.Domain != "" || ck.MaxAge != 600 {
					t.Errorf("cookie = %+v, want HttpOnly, secure=%v, SameSite=%s", ck, tc.secure, tc.sameSite)
				}
			}
			if !found {
				t.Fatalf("no %s cookie set", wantName)
			}
		})
	}
}

func TestSAMLDisabledLeavesRoutesOut(t *testing.T) {
	ts, s := newTestServer(t)
	if s.saml != nil {
		t.Fatal("SAML client built without configuration")
	}
	for _, p := range []string{"/v1/auth/saml/login", "/v1/auth/saml/metadata"} {
		expectStatus(t, get(t, browser(t), ts.URL+p), http.StatusNotFound)
	}
	if code := post(t, browser(t), ts.URL+"/v1/auth/saml/acs", "application/x-www-form-urlencoded", nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to the unregistered ACS answered %d", code)
	}
}

func TestSAMLOnlyDisablesLocalLogin(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, func(c *config.Config) { c.LocalLogin = false })
	creds := []byte(`{"email":"a@example.com","password":"correct horse"}`)
	expectStatus(t, post(t, newClient(t), ts.ts.URL+"/v1/user/login", "application/json", creds), http.StatusNotFound)
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
}

func TestSAMLIdPDownAndRecovery(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, nil)
	idp.mu.Lock()
	idp.metaCode = http.StatusInternalServerError
	idp.mu.Unlock()
	code, _, _ := oidcGet(t, browser(t), ts.ts.URL+"/v1/auth/saml/login")
	expectStatus(t, code, http.StatusBadGateway)
	// Failures are not retried at once...
	idp.mu.Lock()
	idp.metaCode = http.StatusOK
	idp.mu.Unlock()
	code, _, _ = oidcGet(t, browser(t), ts.ts.URL+"/v1/auth/saml/login")
	expectStatus(t, code, http.StatusBadGateway)
	// ...but are, after the cooldown.
	ts.s.saml.fetchMu.Lock()
	ts.s.saml.retryFrom = time.Time{}
	ts.s.saml.fetchMu.Unlock()
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
}

func TestSAMLMetadataRefreshAndRotation(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, nil)
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)

	age := func() {
		ts.s.saml.fetchMu.Lock()
		ts.s.saml.fetched = time.Now().Add(-2 * samlMetadataTTL)
		ts.s.saml.fetchMu.Unlock()
	}
	// The IdP rotates its key. Until the cached metadata expires, the new key is unknown.
	idp.rotate(t)
	idp.update(func(s *samlSettings) { s.assertionID = "" })
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusUnauthorized)
	age()
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)

	// A failed refresh keeps serving the metadata already held.
	idp.mu.Lock()
	idp.metaCode = http.StatusServiceUnavailable
	idp.mu.Unlock()
	age()
	expectStatus(t, ts.signIn(t, browser(t)), http.StatusOK)
}

func TestSAMLBadMetadata(t *testing.T) {
	for name, doc := range map[string]string{
		"not xml":     "hello",
		"no idp":      `<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="x"/>`,
		"no key":      `<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="x"><md:IDPSSODescriptor><md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example/sso"/></md:IDPSSODescriptor></md:EntityDescriptor>`,
		"no redirect": `<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="x"><md:IDPSSODescriptor/></md:EntityDescriptor>`,
		"bad cert":    `<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" entityID="x"><md:IDPSSODescriptor><md:KeyDescriptor><ds:KeyInfo><ds:X509Data><ds:X509Certificate>AAAA</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example/sso"/></md:IDPSSODescriptor></md:EntityDescriptor>`,
		"bad base64":  `<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" entityID="x"><md:IDPSSODescriptor><md:KeyDescriptor><ds:KeyInfo><ds:X509Data><ds:X509Certificate>!!</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example/sso"/></md:IDPSSODescriptor></md:EntityDescriptor>`,
	} {
		t.Run(name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "idp.xml")
			if err := os.WriteFile(f, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := newSAMLClient(config.SAMLConfig{Enabled: true, IDPMetadataFile: f, EntityID: "sp", ACSURL: "https://p.example/acs"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.provider(t.Context()); err == nil {
				t.Fatal("unusable metadata accepted")
			}
		})
	}
}

func TestSAMLBounds(t *testing.T) {
	c, err := newSAMLClient(config.SAMLConfig{Enabled: true, IDPMetadataURL: "https://idp.example/m", EntityID: "sp", ACSURL: "https://p.example/acs"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < samlMaxPending; i++ {
		if _, ok := c.begin(fmt.Sprint("id", i), ""); !ok {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if _, ok := c.begin("one-too-many", ""); ok {
		t.Fatal("pending attempts are not bounded")
	}
	// An attempt taken with the wrong cookie stays; with the right one it is used up once.
	if _, ok := c.take("id1", "wrong"); ok {
		t.Fatal("took an attempt with the wrong cookie")
	}
	cookie := c.pending["id1"].cookie
	if _, ok := c.take("id1", cookie); !ok {
		t.Fatal("could not take the attempt with its own cookie")
	}
	if _, ok := c.take("id1", cookie); ok {
		t.Fatal("attempt taken twice")
	}
	// Expired attempts are not usable.
	c.pending["id2"] = samlPending{cookie: "c", expires: time.Now().Add(-time.Second)}
	if _, ok := c.take("id2", "c"); ok {
		t.Fatal("took an expired attempt")
	}

	until := time.Now().Add(time.Minute)
	for i := 0; i < samlMaxUsed; i++ {
		if err := c.markUsed(fmt.Sprint("a", i), until); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.markUsed("a1", until); !errors.Is(err, errSAMLReplay) {
		t.Fatalf("replay not detected: %v", err)
	}
	if err := c.markUsed("overflow", until); !errors.Is(err, errSAMLBusy) {
		t.Fatalf("used-ID cache is not bounded: %v", err)
	}
	// Entries are forgotten once their window has passed.
	c.used["a1"] = time.Now().Add(-time.Second)
	if err := c.markUsed("a1", until); err != nil {
		t.Fatalf("expired entry still blocks: %v", err)
	}
	for k := range c.used {
		c.used[k] = time.Now().Add(-time.Second)
	}
	if err := c.markUsed("fresh", until); err != nil {
		t.Fatalf("a full cache of expired entries refused a new one: %v", err)
	}
	for k := range c.pending {
		p := c.pending[k]
		p.expires = time.Now().Add(-time.Second)
		c.pending[k] = p
	}
	if _, ok := c.begin("fresh", ""); !ok {
		t.Fatal("a full pending map of expired attempts refused a new one")
	}
}

func TestIndexShowsSAMLSignIn(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, func(c *config.Config) { c.LocalLogin = false })
	_, body, _ := oidcGet(t, browser(t), ts.ts.URL+"/index")
	if !strings.Contains(body, `href="/v1/auth/saml/login?return_to=/index"`) || strings.Contains(body, "oidc/login") ||
		strings.Contains(body, `action="/v1/user/login"`) || strings.Contains(body, "(SAML)") {
		t.Errorf("SAML-only page wrong:\n%s", body)
	}

	// With both providers on, the buttons are told apart.
	oidcIDP := newFakeIDP(t)
	ts = newSAMLTestServer(t, idp, func(c *config.Config) {
		c.OIDC = config.OIDCConfig{
			Enabled: true, Issuer: oidcIDP.srv.URL, ClientID: testClientID, ClientSecret: testClientSecret,
			RedirectURL: testRedirectURL, Scopes: []string{"openid"}, EmailClaim: "email",
		}
	})
	_, body, _ = oidcGet(t, browser(t), ts.ts.URL+"/index")
	for _, want := range []string{`href="/v1/auth/saml/login?return_to=/index"`, `href="/v1/auth/oidc/login?return_to=/index"`, "(SAML)", "(OpenID Connect)"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
