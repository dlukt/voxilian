package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/jwx-go/jwkfetch/v4"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jwt"
)

// jwksMaxBodySize bounds the one-shot startup JWKS response body at
// 1 MiB. A Keycloak JWKS is nowhere near that size; the ceiling exists
// so a compromised/misconfigured endpoint cannot exhaust memory.
const jwksMaxBodySize = 1 << 20

// ErrInvalidToken marks every user-token validation failure: bad
// signature, unknown kid, wrong issuer/audience, missing or expired
// exp, missing/empty sub, malformed JWT. Match with errors.Is; the
// wrapped library cause stays available server-side via errors.As/%v
// but raw cryptographic detail never reaches clients.
var ErrInvalidToken = errors.New("invalid_token")

// Clock supplies time for expiry/deadline comparisons. Production uses
// the wall clock; tests inject manual time. jwt.Clock is never exposed:
// any Now() time.Time source (including simtest clocks) adapts to this
// seam without modification.
type Clock interface {
	Now() time.Time
}

// systemClock is the production Clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Identity is the validated access-token identity handed to the
// gateway. Email is optional: HasEmail is false when the claim is
// absent (or empty); identity is always the Keycloak sub, never email.
type Identity struct {
	Sub       string
	Email     string
	HasEmail  bool
	ExpiresAt time.Time
}

// Validator validates an access token and returns its identity. Any
// failure wraps ErrInvalidToken.
type Validator interface {
	Validate(ctx context.Context, accessToken string) (Identity, error)
}

// ValidatorFunc adapts a plain function to a Validator (nil-guard
// defaults, tests).
type ValidatorFunc func(ctx context.Context, accessToken string) (Identity, error)

// Validate implements Validator.
func (f ValidatorFunc) Validate(ctx context.Context, accessToken string) (Identity, error) {
	return f(ctx, accessToken)
}

// ValidatorConfig is the trusted validator input (spec §6.2.1). Issuer,
// Audience, and JWKSURL are all required: issuer comparison is exact,
// the expected audience must appear in the JWT aud claim, and the JWKS
// URL comes only from trusted configuration — never from JWT headers,
// with no OIDC discovery. HTTPClient and Clock are injectable seams
// for deterministic tests; nil means http.DefaultClient / wall clock.
type ValidatorConfig struct {
	Issuer     string
	Audience   string
	JWKSURL    string
	HTTPClient *http.Client
	Clock      Clock
}

// JWKSValidator is the M3 startup-JWKS baseline: one real HTTP fetch at
// construction, the parsed set held immutable in memory, no per-token
// network I/O, no background refresh (M11 upgrades this layer).
type JWKSValidator struct {
	issuer   string
	audience string
	keys     jwk.Set
	clock    Clock
}

var _ Validator = (*JWKSValidator)(nil)

// NewJWKSValidator fetches the trusted JWKS exactly once and retains
// it. The configured URL is the only permitted fetch target
// (exact-allowlist, including redirect targets for *http.Client); any
// fetch/parse failure returns an error — never a validator that
// silently accepts nothing or skips verification.
func NewJWKSValidator(ctx context.Context, cfg ValidatorConfig) (*JWKSValidator, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("auth: issuer is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("auth: audience is required")
	}
	if cfg.JWKSURL == "" {
		return nil, errors.New("auth: jwksURL is required")
	}
	u, err := url.Parse(cfg.JWKSURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("auth: invalid jwksURL %q: must be an absolute http(s) URL", cfg.JWKSURL)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clock := cfg.Clock
	if clock == nil {
		clock = systemClock{}
	}
	client := jwkfetch.NewClient(
		jwkfetch.WithWhitelist(jwkfetch.NewMapWhitelist().Add(cfg.JWKSURL)),
		jwkfetch.WithHTTPClient(httpClient),
		jwkfetch.WithMaxBodySize(jwksMaxBodySize),
	)
	set, err := client.Fetch(ctx, cfg.JWKSURL)
	if err != nil {
		return nil, fmt.Errorf("auth: fetch JWKS: %w", err)
	}
	return &JWKSValidator{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		keys:     set,
		clock:    clock,
	}, nil
}

// Validate verifies the token against the pre-fetched trusted set and
// returns its identity. Signature, kid-selected key with trusted-JWK
// algorithm metadata, exact issuer, audience containment, required
// exp/sub, future exp, and present nbf/iat are all enforced on the
// injected clock. No fetch happens here: repeated calls reuse the
// immutable startup set, and token-controlled jku/x5u URLs are never
// contacted because key lookup never leaves the set.
func (v *JWKSValidator) Validate(_ context.Context, accessToken string) (Identity, error) {
	tok, err := jwt.Parse([]byte(accessToken),
		jwt.WithKeySet(v.keys),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithRequiredClaim(jwt.ExpirationKey),
		jwt.WithRequiredClaim(jwt.SubjectKey),
		jwt.WithClock(jwt.ClockFunc(v.clock.Now)),
	)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: validate token: %v: %w", err, ErrInvalidToken)
	}
	sub, _ := tok.Subject()
	if sub == "" {
		return Identity{}, fmt.Errorf("auth: empty sub: %w", ErrInvalidToken)
	}
	exp, _ := tok.Expiration()
	id := Identity{Sub: sub, ExpiresAt: exp}
	if raw, ok := tok.Field("email"); ok {
		s, ok := raw.(string)
		if !ok {
			return Identity{}, fmt.Errorf("auth: non-string email claim: %w", ErrInvalidToken)
		}
		if s != "" {
			id.Email, id.HasEmail = s, true
		}
	}
	return id, nil
}
