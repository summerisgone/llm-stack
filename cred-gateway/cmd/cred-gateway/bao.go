package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var errNoConnection = errors.New("no connection")

// store returns the user's credential for a connector. The gateway reads
// through it so OpenBao can be swapped (docs/adr/0024, alternatives).
type store interface {
	Token(ctx context.Context, userID, connector string) (string, error)
}

// bao reads secret/data/users/<id>/<connector> from OpenBao KV v2 with a
// client token obtained by Kubernetes auth. The role grants read on its
// connector paths only (docs/adr/0024 decision 1).
type bao struct {
	addr, mount, role, jwtFile string
	client                     *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (b *bao) login(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && time.Now().Before(b.expires) {
		return b.token, nil
	}
	// Projected ServiceAccount tokens rotate; read the current one.
	jwt, err := os.ReadFile(b.jwtFile)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"role": b.role, "jwt": strings.TrimSpace(string(jwt))})
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := b.do(ctx, http.MethodPost, "/v1/auth/kubernetes/login", "", body, &out); err != nil {
		return "", fmt.Errorf("openbao login: %w", err)
	}
	b.token = out.Auth.ClientToken
	// Renew well before expiry instead of handling a mid-request 403.
	b.expires = time.Now().Add(time.Duration(out.Auth.LeaseDuration) * time.Second * 3 / 4)
	return b.token, nil
}

func (b *bao) Token(ctx context.Context, userID, connector string) (string, error) {
	token, err := b.login(ctx)
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	path := "/v1/" + b.mount + "/data/users/" + url.PathEscape(userID) + "/" + url.PathEscape(connector)
	err = b.do(ctx, http.MethodGet, path, token, nil, &out)
	if errors.Is(err, errNoConnection) {
		return "", err
	}
	if err != nil {
		b.mu.Lock()
		b.token = ""
		b.mu.Unlock()
		return "", err
	}
	if out.Data.Data["token"] == "" {
		return "", errNoConnection
	}
	return out.Data.Data["token"], nil
}

func (b *bao) do(ctx context.Context, method, path, token string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, b.addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNoConnection
	}
	if resp.StatusCode >= 300 {
		// OpenBao error bodies name paths, never secret values.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
