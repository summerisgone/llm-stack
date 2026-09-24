package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testIssuer = "http://sso.test/realms/ai-stack"

func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	head := enc(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	body := enc(claims)
	digest := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func jwksServer(t *testing.T, key *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": kid, "kty": "RSA", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func claims(roles ...string) map[string]any {
	return map[string]any{
		"iss": testIssuer, "sub": "user-sub-1", "preferred_username": "demo",
		"exp":          time.Now().Add(5 * time.Minute).Unix(),
		"realm_access": map[string]any{"roles": roles},
	}
}

func TestVerify(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := NewVerifier(testIssuer, jwksServer(t, key, "k1").URL, []string{"ai-user", "ai-admin"})
	ctx := context.Background()

	u, err := v.Verify(ctx, signToken(t, key, "k1", claims("ai-user")))
	if err != nil || u.Sub != "user-sub-1" || u.Name != "demo" || u.ID != profileID("user-sub-1") || len(u.ID) != 16 {
		t.Fatalf("valid token: %+v %v", u, err)
	}

	expired := claims("ai-user")
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	wrongIss := claims("ai-user")
	wrongIss["iss"] = "http://evil/realms/ai-stack"
	cases := map[string]struct {
		authz string
		want  error
	}{
		"missing":         {"", errUnauthenticated},
		"not bearer":      {"Basic abc", errUnauthenticated},
		"garbage":         {"Bearer a.b.c", errUnauthenticated},
		"wrong signature": {signToken(t, other, "k1", claims("ai-user")), errUnauthenticated},
		"unknown kid":     {signToken(t, key, "k2", claims("ai-user")), errUnauthenticated},
		"expired":         {signToken(t, key, "k1", expired), errUnauthenticated},
		"wrong issuer":    {signToken(t, key, "k1", wrongIss), errUnauthenticated},
		"no allowed role": {signToken(t, key, "k1", claims("offline_access")), errForbidden},
	}
	for name, c := range cases {
		if _, err := v.Verify(ctx, c.authz); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}
