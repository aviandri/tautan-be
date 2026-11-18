package server

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// oidcClient is a minimal OpenID Connect relying party for the authorization
// code flow: discovery, the token exchange and ID token verification against
// the provider's JWKS. Discovery and keys are cached.
type oidcClient struct {
	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string
	http         *http.Client
	now          func() time.Time

	mu     sync.Mutex
	meta   *oidcMeta
	metaAt time.Time
	keys   map[string]*rsa.PublicKey
	keysAt time.Time
}

type oidcMeta struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// idClaims are the ID token claims Tautan uses.
type idClaims struct {
	jwt.RegisteredClaims
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"` // bool, but some providers send "true"
	Name          string `json:"name"`
	Nonce         string `json:"nonce"`
}

func (c idClaims) emailVerified() bool {
	switch v := c.EmailVerified.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

const (
	oidcMetaTTL = 24 * time.Hour
	oidcKeysTTL = time.Hour
	// Refetch the JWKS for an unknown kid at most this often (key rotation).
	oidcKeysMinRefresh = time.Minute
)

var (
	errOIDCProvider   = errors.New("identity provider error")
	errOIDCBadCode    = errors.New("authorization code rejected")
	errOIDCBadIDToken = errors.New("invalid id token")
)

func newOIDCClient(issuer, clientID, clientSecret, redirectURL string, now func() time.Time) *oidcClient {
	return &oidcClient{
		issuer: issuer, clientID: clientID, clientSecret: clientSecret, redirectURL: redirectURL,
		http: &http.Client{Timeout: 10 * time.Second}, now: now,
	}
}

func (c *oidcClient) getJSON(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", u, res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(dst)
}

// discover returns the provider metadata, fetching it when stale.
func (c *oidcClient) discover(ctx context.Context) (*oidcMeta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.meta != nil && c.now().Sub(c.metaAt) < oidcMetaTTL {
		return c.meta, nil
	}
	var m oidcMeta
	if err := c.getJSON(ctx, c.issuer+"/.well-known/openid-configuration", &m); err != nil {
		return nil, fmt.Errorf("%w: discovery: %v", errOIDCProvider, err)
	}
	if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" || m.JWKSURI == "" {
		return nil, fmt.Errorf("%w: discovery document incomplete", errOIDCProvider)
	}
	if m.Issuer != c.issuer {
		return nil, fmt.Errorf("%w: discovery issuer %q does not match %q", errOIDCProvider, m.Issuer, c.issuer)
	}
	c.meta, c.metaAt = &m, c.now()
	return c.meta, nil
}

// authURL builds the authorization request URL (PKCE S256).
func (c *oidcClient) authURL(meta *oidcMeta, state, nonce, challenge string) (string, error) {
	u, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", c.redirectURL)
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("prompt", "select_account")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// exchange redeems an authorization code and returns the verified ID token claims.
func (c *oidcClient) exchange(ctx context.Context, code, verifier, nonce string) (*idClaims, error) {
	meta, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.redirectURL},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: token request: %v", errOIDCProvider, err)
	}
	defer res.Body.Close()
	var body struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body)
	if res.StatusCode != http.StatusOK {
		// invalid_grant: the code expired, was already used, or the verifier is wrong.
		if body.Error == "invalid_grant" {
			return nil, errOIDCBadCode
		}
		return nil, fmt.Errorf("%w: token endpoint status %d (%s)", errOIDCProvider, res.StatusCode, body.Error)
	}
	if body.IDToken == "" {
		return nil, fmt.Errorf("%w: no id_token in token response", errOIDCProvider)
	}
	return c.verify(ctx, meta, body.IDToken, nonce)
}

func (c *oidcClient) verify(ctx context.Context, meta *oidcMeta, raw, nonce string) (*idClaims, error) {
	claims := &idClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return c.key(ctx, meta, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithAudience(c.clientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(time.Minute),
		jwt.WithTimeFunc(c.now),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errOIDCBadIDToken, err)
	}
	// Google issues both "https://accounts.google.com" and "accounts.google.com".
	if iss := claims.Issuer; iss != meta.Issuer && iss != strings.TrimPrefix(meta.Issuer, "https://") {
		return nil, fmt.Errorf("%w: issuer %q", errOIDCBadIDToken, iss)
	}
	if claims.Nonce == "" || claims.Nonce != nonce {
		return nil, fmt.Errorf("%w: nonce mismatch", errOIDCBadIDToken)
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: missing sub", errOIDCBadIDToken)
	}
	return claims, nil
}

// key returns the RSA key for kid, refetching the JWKS when it is stale or the
// kid is unknown (rotation).
func (c *oidcClient) key(ctx context.Context, meta *oidcMeta, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	age := c.now().Sub(c.keysAt)
	if k, ok := c.keys[kid]; ok && age < oidcKeysTTL {
		return k, nil
	}
	if c.keys != nil && age < oidcKeysMinRefresh {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	var set struct {
		Keys []struct {
			Kty, Kid, Use, N, E string
		} `json:"keys"`
	}
	if err := c.getJSON(ctx, meta.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("%w: jwks: %v", errOIDCProvider, err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	c.keys, c.keysAt = keys, c.now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown key id %q", kid)
}
