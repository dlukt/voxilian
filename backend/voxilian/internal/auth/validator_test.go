package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jws"
	"github.com/lestrrat-go/jwx/v4/jwt"

	"github.com/dlukt/voxilian/internal/auth"
	"github.com/dlukt/voxilian/internal/simtest"
)

const (
	testIssuer   = "https://keycloak.test/realms/vox"
	testAudience = "voxilian"
	testKid      = "test-kid-1"
)

type keyWorld struct {
	priv *rsa.PrivateKey
	evil *rsa.PrivateKey
	set  jwk.Set
	hits *atomic.Int32
	srv  *httptest.Server
	now  time.Time
}

func newKeyWorld(t *testing.T) *keyWorld {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	evil, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate evil key: %v", err)
	}
	pub, err := jwk.PublicKeyOf(priv)
	if err != nil {
		t.Fatalf("public JWK: %v", err)
	}
	if err := pub.Set(jwk.KeyIDKey, testKid); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := pub.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		t.Fatalf("set alg: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pub); err != nil {
		t.Fatalf("add key: %v", err)
	}
	w := &keyWorld{
		priv: priv,
		evil: evil,
		set:  set,
		hits: &atomic.Int32{},
		now:  time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
	}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		w.hits.Add(1)
		rw.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(rw).Encode(set); err != nil {
			t.Errorf("serve JWKS: %v", err)
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *keyWorld) newValidator(t *testing.T, clock auth.Clock) *auth.JWKSValidator {
	t.Helper()
	v, err := auth.NewJWKSValidator(context.Background(), auth.ValidatorConfig{
		Issuer:   testIssuer,
		Audience: testAudience,
		JWKSURL:  w.srv.URL,
		Clock:    clock,
	})
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}
	return v
}

// sign mints a Keycloak-like RS256 token. opts mutate the claim set;
// key+kah kid select the signing material and header kid. Default
// iat/exp anchor on the world's fixed instant so tokens validate
// against a fixed test clock without wall-clock skew.
func (w *keyWorld) sign(
	t *testing.T,
	key *rsa.PrivateKey,
	kid string,
	mutate func(tok jwt.Token),
) string {
	t.Helper()
	now := w.now
	tok := jwt.New()
	_ = tok.Set(jwt.IssuerKey, testIssuer)
	_ = tok.Set(jwt.AudienceKey, []string{testAudience})
	_ = tok.Set(jwt.SubjectKey, "hero-sub")
	_ = tok.Set(jwt.ExpirationKey, now.Add(time.Hour))
	_ = tok.Set(jwt.IssuedAtKey, now)
	if mutate != nil {
		mutate(tok)
	}
	hdrs := jws.NewHeaders()
	if err := hdrs.Set(jws.KeyIDKey, kid); err != nil {
		t.Fatalf("set header kid: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), key, jws.WithProtectedHeaders(hdrs)))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return string(signed)
}

// valid signs a fully-valid token with the trusted key/kid.
func (w *keyWorld) valid(t *testing.T, mutate func(jwt.Token)) string {
	t.Helper()
	return w.sign(t, w.priv, testKid, mutate)
}

func (w *keyWorld) clock() *simtest.Clock {
	return simtest.NewClock(w.now)
}

func TestValidatorAcceptsValidToken(t *testing.T) {
	w := newKeyWorld(t)
	clock := w.clock()
	now := w.now
	v := w.newValidator(t, clock)
	id, err := v.Validate(context.Background(), w.valid(t, func(tok jwt.Token) {
		_ = tok.Set("email", "hero@example.com")
		_ = tok.Set(jwt.ExpirationKey, now.Add(5*time.Minute))
	}))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.Sub != "hero-sub" {
		t.Errorf("sub = %q", id.Sub)
	}
	if !id.HasEmail || id.Email != "hero@example.com" {
		t.Errorf("email = %q,%v", id.Email, id.HasEmail)
	}
	if !id.ExpiresAt.Equal(now.Add(5 * time.Minute)) {
		t.Errorf("exp = %v", id.ExpiresAt)
	}
}

func TestValidatorEmailOptional(t *testing.T) {
	w := newKeyWorld(t)
	clock := w.clock()
	v := w.newValidator(t, clock)
	id, err := v.Validate(context.Background(), w.valid(t, nil))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.HasEmail || id.Email != "" {
		t.Errorf("email = %q,%v, want absent", id.Email, id.HasEmail)
	}
	// Empty string counts as absent, not as an email.
	id, err = v.Validate(context.Background(), w.valid(t, func(tok jwt.Token) {
		_ = tok.Set("email", "")
	}))
	if err != nil {
		t.Fatalf("Validate empty email: %v", err)
	}
	if id.HasEmail {
		t.Error("empty email must count as absent")
	}
}

func TestValidatorRejects(t *testing.T) {
	w := newKeyWorld(t)
	clock := w.clock()
	now := w.now

	cases := map[string]func(*testing.T) string{
		"forged signature": func(t *testing.T) string {
			return w.sign(t, w.evil, testKid, nil)
		},
		"unknown kid": func(t *testing.T) string {
			return w.sign(t, w.evil, "evil-kid", nil)
		},
		"expired": func(t *testing.T) string {
			return w.valid(t, func(tok jwt.Token) {
				_ = tok.Set(jwt.ExpirationKey, now.Add(-time.Minute))
			})
		},
		"wrong issuer": func(t *testing.T) string {
			return w.valid(t, func(tok jwt.Token) {
				_ = tok.Set(jwt.IssuerKey, "https://evil.test/realms/vox")
			})
		},
		"wrong audience": func(t *testing.T) string {
			return w.valid(t, func(tok jwt.Token) {
				_ = tok.Set(jwt.AudienceKey, []string{"other-game"})
			})
		},
		"missing exp": func(t *testing.T) string {
			tok := jwt.New()
			_ = tok.Set(jwt.IssuerKey, testIssuer)
			_ = tok.Set(jwt.AudienceKey, []string{testAudience})
			_ = tok.Set(jwt.SubjectKey, "hero-sub")
			hdrs := jws.NewHeaders()
			_ = hdrs.Set(jws.KeyIDKey, testKid)
			signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), w.priv, jws.WithProtectedHeaders(hdrs)))
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			return string(signed)
		},
		"missing sub": func(t *testing.T) string {
			tok := jwt.New()
			_ = tok.Set(jwt.IssuerKey, testIssuer)
			_ = tok.Set(jwt.AudienceKey, []string{testAudience})
			_ = tok.Set(jwt.ExpirationKey, now.Add(time.Hour))
			hdrs := jws.NewHeaders()
			_ = hdrs.Set(jws.KeyIDKey, testKid)
			signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), w.priv, jws.WithProtectedHeaders(hdrs)))
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			return string(signed)
		},
		"empty sub": func(t *testing.T) string {
			return w.valid(t, func(tok jwt.Token) {
				_ = tok.Set(jwt.SubjectKey, "")
			})
		},
		"future nbf": func(t *testing.T) string {
			return w.valid(t, func(tok jwt.Token) {
				_ = tok.Set(jwt.NotBeforeKey, now.Add(time.Hour))
			})
		},
		"malformed": func(t *testing.T) string {
			return "not.a.jwt"
		},
		"non-string email": func(t *testing.T) string {
			return w.valid(t, func(tok jwt.Token) {
				_ = tok.Set("email", 42)
			})
		},
	}
	for name, mint := range cases {
		t.Run(name, func(t *testing.T) {
			v := w.newValidator(t, clock)
			_, err := v.Validate(context.Background(), mint(t))
			if !errors.Is(err, auth.ErrInvalidToken) {
				t.Errorf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestValidatorRejectsNoneAlg(t *testing.T) {
	w := newKeyWorld(t)
	clock := w.clock()
	v := w.newValidator(t, clock)
	enc := base64.RawURLEncoding.EncodeToString
	unsigned := enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
		enc([]byte(fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":"x","exp":9999999999}`,
			testIssuer, testAudience))) + "."
	if _, err := v.Validate(context.Background(), unsigned); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("none-alg err = %v, want ErrInvalidToken", err)
	}
}

func TestValidatorOneShotJWKS(t *testing.T) {
	w := newKeyWorld(t)
	clock := w.clock()
	if n := w.hits.Load(); n != 0 {
		t.Fatalf("hits = %d before construction", n)
	}
	v := w.newValidator(t, clock)
	if n := w.hits.Load(); n != 1 {
		t.Fatalf("hits after construction = %d, want exactly 1", n)
	}
	for i := 0; i < 3; i++ {
		if _, err := v.Validate(context.Background(), w.valid(t, nil)); err != nil {
			t.Fatalf("validate #%d: %v", i, err)
		}
	}
	if n := w.hits.Load(); n != 1 {
		t.Errorf("hits after 3 validations = %d, want still 1 (immutable startup set)", n)
	}
}

func TestValidatorIgnoresAttackerJKU(t *testing.T) {
	w := newKeyWorld(t)
	clock := w.clock()
	v := w.newValidator(t, clock)
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		evilHits.Add(1)
		rw.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()

	now := w.now
	tok := jwt.New()
	_ = tok.Set(jwt.IssuerKey, testIssuer)
	_ = tok.Set(jwt.AudienceKey, []string{testAudience})
	_ = tok.Set(jwt.SubjectKey, "hero-sub")
	_ = tok.Set(jwt.ExpirationKey, now.Add(time.Hour))
	hdrs := jws.NewHeaders()
	_ = hdrs.Set(jws.KeyIDKey, testKid)
	_ = hdrs.Set("jku", evil.URL)
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), w.priv, jws.WithProtectedHeaders(hdrs)))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, verr := v.Validate(context.Background(), string(signed))
	_ = verr // validity is orthogonal; the invariant is no token-driven fetch.
	if n := evilHits.Load(); n != 0 {
		t.Errorf("attacker jku contacted %d times, want 0", n)
	}
}

func TestValidatorConfigRejected(t *testing.T) {
	w := newKeyWorld(t)
	for name, cfg := range map[string]auth.ValidatorConfig{
		"empty issuer":   {Issuer: "", Audience: testAudience, JWKSURL: w.srv.URL},
		"empty audience": {Issuer: testIssuer, Audience: "", JWKSURL: w.srv.URL},
		"empty jwksURL":  {Issuer: testIssuer, Audience: testAudience, JWKSURL: ""},
		"bad scheme":     {Issuer: testIssuer, Audience: testAudience, JWKSURL: "ftp://x/jwks.json"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := auth.NewJWKSValidator(context.Background(), cfg); err == nil {
				t.Error("expected constructor error")
			} else if errors.Is(err, auth.ErrInvalidToken) {
				t.Errorf("config error must not be ErrInvalidToken: %v", err)
			}
		})
	}
}

func TestValidatorFetchFailure(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	dead.Close() // nothing listens: fetch must fail.
	_, err := auth.NewJWKSValidator(context.Background(), auth.ValidatorConfig{
		Issuer: testIssuer, Audience: testAudience, JWKSURL: dead.URL,
	})
	if err == nil {
		t.Fatal("expected fetch error")
	}
	if errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("fetch error must not be ErrInvalidToken: %v", err)
	}
}
