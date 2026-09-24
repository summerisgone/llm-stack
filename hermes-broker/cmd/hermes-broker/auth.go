package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// User is the identity taken from a validated Keycloak access token. The
// X-User-Id / X-User-Name headers Open WebUI also sends are never read.
type User struct {
	Sub  string
	Name string
	// ID is the profile key: first 16 hex chars of sha256(sub). DNS-safe and
	// does not expose the sub in object names.
	ID string
}

func profileID(sub string) string {
	sum := sha256.Sum256([]byte(sub))
	return hex.EncodeToString(sum[:])[:16]
}

var (
	errUnauthenticated = errors.New("missing or invalid token")
	errForbidden       = errors.New("token has no allowed role")
)

type Verifier struct {
	issuer  string
	jwksURL string
	roles   []string
	client  *http.Client
	now     func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func NewVerifier(issuer, jwksURL string, roles []string) *Verifier {
	return &Verifier{issuer: issuer, jwksURL: jwksURL, roles: roles,
		client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
}

type jwtClaims struct {
	Iss         string `json:"iss"`
	Sub         string `json:"sub"`
	Exp         int64  `json:"exp"`
	Nbf         int64  `json:"nbf"`
	Username    string `json:"preferred_username"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (v *Verifier) Verify(ctx context.Context, authz string) (User, error) {
	token, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || token == "" {
		return User{}, errUnauthenticated
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return User{}, errUnauthenticated
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Alg != "RS256" {
		return User{}, errUnauthenticated
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return User{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return User{}, errUnauthenticated
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return User{}, errUnauthenticated
	}
	var c jwtClaims
	if err := decodeSegment(parts[1], &c); err != nil {
		return User{}, errUnauthenticated
	}
	now := v.now().Unix()
	const leeway = 30
	if c.Iss != v.issuer || c.Sub == "" || c.Exp == 0 || now > c.Exp+leeway || (c.Nbf != 0 && now+leeway < c.Nbf) {
		return User{}, errUnauthenticated
	}
	if !slices.ContainsFunc(c.RealmAccess.Roles, func(r string) bool { return slices.Contains(v.roles, r) }) {
		return User{}, errForbidden
	}
	return User{Sub: c.Sub, Name: c.Username, ID: profileID(c.Sub)}, nil
}

func decodeSegment(seg string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

// key returns the signing key for kid, refetching the JWKS on an unknown kid
// (Keycloak key rotation) at most once every 30 seconds.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k := v.keys[kid]; k != nil {
		return k, nil
	}
	if v.now().Sub(v.fetchedAt) < 30*time.Second && v.keys != nil {
		return nil, errUnauthenticated
	}
	keys, err := v.fetch(ctx)
	v.fetchedAt = v.now()
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	v.keys = keys
	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, errUnauthenticated
}

func (v *Verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return keys, nil
}
