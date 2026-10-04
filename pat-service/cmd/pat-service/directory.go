package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// directory reads users and their effective realm roles from Keycloak with
// the read-only pat-directory service account (docs/adr/0022 section 1). It
// holds realm-management view-users only: it cannot grant roles, reset
// passwords or administer Keycloak.
type directory struct {
	adminBase string // <keycloak>/admin/realms/<realm>
	tokenURL  string
	clientID  string
	secret    string
	http      *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	members  map[string]adminMembership
}

type adminMembership struct {
	admin   bool
	checked time.Time
}

// adminMembershipTTL bounds how long a read may trust a previous check.
const adminMembershipTTL = 60 * time.Second

var errDirectoryUnavailable = errors.New("user directory unavailable")

type directoryUser struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Enabled   bool   `json:"enabled"`
	Created   int64  `json:"createdTimestamp"`
}

// newDirectory returns nil when no directory secret is configured; the
// admin console then stays unavailable instead of trusting cookie roles.
func newDirectory(internalIssuer, clientID, secret string, client *http.Client) *directory {
	if secret == "" {
		return nil
	}
	return &directory{
		adminBase: strings.Replace(internalIssuer, "/realms/", "/admin/realms/", 1),
		tokenURL:  internalIssuer + "/protocol/openid-connect/token",
		clientID:  clientID,
		secret:    secret,
		http:      client,
		members:   map[string]adminMembership{},
	}
}

func (d *directory) bearer(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token != "" && time.Now().Add(30*time.Second).Before(d.tokenExp) {
		return d.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {d.clientID}, "client_secret": {d.secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("directory token endpoint returned %s", resp.Status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("directory token endpoint returned no access token")
	}
	d.token = out.AccessToken
	d.tokenExp = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return d.token, nil
}

// get returns false without error when Keycloak answers 404.
func (d *directory) get(ctx context.Context, path string, query url.Values, out any) (bool, error) {
	token, err := d.bearer(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errDirectoryUnavailable, err)
	}
	target := d.adminBase + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errDirectoryUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%w: %s returned %s", errDirectoryUnavailable, path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, fmt.Errorf("%w: %v", errDirectoryUnavailable, err)
	}
	return true, nil
}

// realmRoles returns the user's effective realm roles, composites expanded.
func (d *directory) realmRoles(ctx context.Context, id string) ([]string, error) {
	var roles []struct {
		Name string `json:"name"`
	}
	if _, err := d.get(ctx, "/users/"+url.PathEscape(id)+"/role-mappings/realm/composite", nil, &roles); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, r.Name)
	}
	return names, nil
}

// isAdmin reports whether the Keycloak user is enabled and effectively holds
// ai-admin. fresh skips the cache; mutations always pass true.
func (d *directory) isAdmin(ctx context.Context, subject string, fresh bool) (bool, error) {
	d.mu.Lock()
	cached, ok := d.members[subject]
	d.mu.Unlock()
	if !fresh && ok && time.Since(cached.checked) < adminMembershipTTL {
		return cached.admin, nil
	}
	var user directoryUser
	found, err := d.get(ctx, "/users/"+url.PathEscape(subject), nil, &user)
	if err != nil {
		return false, err
	}
	admin := false
	if found && user.Enabled {
		roles, err := d.realmRoles(ctx, subject)
		if err != nil {
			return false, err
		}
		admin = hasRole(roles, "ai-admin")
	}
	d.mu.Lock()
	d.members[subject] = adminMembership{admin: admin, checked: time.Now()}
	d.mu.Unlock()
	return admin, nil
}

func (d *directory) listUsers(ctx context.Context, search string, first, max int) ([]directoryUser, int, error) {
	q := url.Values{"first": {strconv.Itoa(first)}, "max": {strconv.Itoa(max)}, "briefRepresentation": {"true"}}
	countQuery := url.Values{}
	if search != "" {
		q.Set("search", search)
		countQuery.Set("search", search)
	}
	users := []directoryUser{}
	if _, err := d.get(ctx, "/users", q, &users); err != nil {
		return nil, 0, err
	}
	var total int
	if _, err := d.get(ctx, "/users/count", countQuery, &total); err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}
