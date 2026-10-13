package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// cred-gateway (docs/adr/0024 decision 3): the agent's requests to a
// connector arrive through pat-service, which has authenticated the agent's
// PAT and set X-User-Id. The gateway reads the user's credential from
// OpenBao, injects it, and forwards to an allowlist of upstream paths. The
// credential never appears in a response. NetworkPolicy admits pat-service
// only, which is why X-User-Id is trusted.

type gateway struct {
	store store
	// Upstream origins; tests point them at httptest servers.
	githubURL, githubAPIURL string
	// denyRefs are refs a push may not update (decision 7 stand-in until
	// the permission round trip exists): the agent pushes branches, a
	// person merges.
	denyRefs map[string]bool
	// connectURL is shown to the user when no connection exists.
	connectURL string
	proxy      http.RoundTripper
}

var (
	// /cred/github/<owner>/<repo>[.git]/<smart-http endpoint>
	gitPath = regexp.MustCompile(`^/cred/github/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)
	// GitHub REST paths the agent may call through /cred/github/api/.
	apiAllow = map[string][]*regexp.Regexp{
		http.MethodGet: {
			regexp.MustCompile(`^/user$`),
			regexp.MustCompile(`^/repos/[^/]+/[^/]+(/(pulls|branches|commits)(/[^/]+)?)?$`),
		},
		http.MethodPost: {
			regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls$`),
		},
	}
	// Request headers worth passing upstream; everything else, Authorization
	// and cookies included, is dropped.
	passHeaders   = []string{"Accept", "Accept-Encoding", "Content-Type", "Content-Encoding", "Git-Protocol", "User-Agent", "X-GitHub-Api-Version"}
	secretPattern = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9_-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
)

// userID is the broker's profile key (agent-broker auth.go): the first 16
// hex chars of sha256(sub). OpenBao paths use it, not the raw subject.
func userID(sub string) string {
	sum := sha256.Sum256([]byte(sub))
	return hex.EncodeToString(sum[:])[:16]
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sub := r.Header.Get("X-User-Id")
	if sub == "" {
		http.Error(w, "missing caller identity", http.StatusUnauthorized)
		return
	}
	var (
		target *url.URL
		basic  bool
		isGit  bool
	)
	switch m := gitPath.FindStringSubmatch(r.URL.Path); {
	case m != nil:
		svc := r.URL.Query().Get("service")
		ok := (m[3] == "info/refs" && r.Method == http.MethodGet && (svc == "git-upload-pack" || svc == "git-receive-pack")) ||
			(m[3] != "info/refs" && r.Method == http.MethodPost)
		if !ok {
			http.NotFound(w, r)
			return
		}
		target, _ = url.Parse(g.githubURL + "/" + m[1] + "/" + m[2] + ".git/" + m[3])
		target.RawQuery = r.URL.RawQuery
		basic, isGit = true, true
	case strings.HasPrefix(r.URL.Path, "/cred/github/api/"):
		rest := strings.TrimPrefix(r.URL.Path, "/cred/github/api")
		if !allowed(r.Method, rest) {
			http.NotFound(w, r)
			return
		}
		target, _ = url.Parse(g.githubAPIURL + rest)
		target.RawQuery = r.URL.RawQuery
	default:
		http.NotFound(w, r)
		return
	}

	status := 0
	defer func() {
		// Never headers, query strings or bodies (decision 8).
		log.Printf("user=%s connector=github method=%s path=%s status=%d", userID(sub), r.Method, r.URL.Path, status)
	}()

	token, err := g.store.Token(r.Context(), userID(sub), "github")
	if errors.Is(err, errNoConnection) {
		status = http.StatusForbidden
		msg := "no GitHub connection for this user; connect one at " + g.connectURL
		if isGit && strings.HasSuffix(r.URL.Path, "/info/refs") {
			// git prints an ERR packet as "remote error: ...", a bare 403
			// only as an HTTP status.
			status = http.StatusOK
			w.Header().Set("Content-Type", "application/x-"+r.URL.Query().Get("service")+"-advertisement")
			io.WriteString(w, pktLine("ERR "+msg))
			return
		}
		http.Error(w, msg, status)
		return
	}
	if err != nil {
		log.Printf("credential store: %v", err)
		status = http.StatusServiceUnavailable
		http.Error(w, "credential store unavailable", status)
		return
	}

	if isGit && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
		body, err := checkPush(r, g.denyRefs)
		if err != nil {
			status = http.StatusForbidden
			http.Error(w, err.Error(), status)
			return
		}
		r.Body = body
		r.Header.Del("Content-Encoding")
		r.ContentLength = -1
	}

	rp := &httputil.ReverseProxy{
		Transport: g.proxy,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = target.Host
			pr.Out.Header = http.Header{}
			for _, h := range passHeaders {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
			if basic {
				pr.Out.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
			} else {
				pr.Out.Header.Set("Authorization", "Bearer "+token)
			}
		},
		// Packs stream; pass them through as they arrive.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			status = resp.StatusCode
			for _, h := range []string{"Authorization", "Set-Cookie", "WWW-Authenticate", "Location"} {
				resp.Header.Del(h)
			}
			if resp.StatusCode >= 400 {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
				clean := scrub(string(raw), token)
				resp.Body = io.NopCloser(strings.NewReader(clean))
				resp.ContentLength = int64(len(clean))
				resp.Header.Set("Content-Length", strconv.Itoa(len(clean)))
				resp.Header.Del("Content-Encoding")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("upstream: %v", err)
			status = http.StatusBadGateway
			http.Error(w, "upstream unavailable", status)
		},
	}
	rp.ServeHTTP(w, r)
}

func allowed(method, path string) bool {
	for _, re := range apiAllow[method] {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

func scrub(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "[REDACTED]")
	}
	return secretPattern.ReplaceAllString(s, "[REDACTED]")
}

func pktLine(s string) string {
	s += "\n"
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

// maxCommands bounds how much of a receive-pack request is buffered to read
// its ref update commands, which precede the pack.
const maxCommands = 1 << 20

// checkPush reads the ref update commands at the start of a receive-pack
// request and refuses a push that updates a denied ref. It returns a body
// that replays the request uncompressed.
func checkPush(r *http.Request, deny map[string]bool) (io.ReadCloser, error) {
	var src io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, errors.New("malformed push request")
		}
		src = zr
	}
	br := bufio.NewReader(src)
	var head bytes.Buffer
	for head.Len() < maxCommands {
		var n [4]byte
		if _, err := io.ReadFull(br, n[:]); err != nil {
			return nil, errors.New("malformed push request")
		}
		head.Write(n[:])
		size, err := strconv.ParseUint(string(n[:]), 16, 16)
		if err != nil {
			return nil, errors.New("malformed push request")
		}
		if size == 0 {
			// Flush: commands end, the pack follows.
			return io.NopCloser(io.MultiReader(bytes.NewReader(head.Bytes()), br)), nil
		}
		if size < 4 {
			return nil, errors.New("malformed push request")
		}
		line := make([]byte, size-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return nil, errors.New("malformed push request")
		}
		head.Write(line)
		cmd, _, _ := bytes.Cut(line, []byte{0})
		f := strings.Fields(string(cmd))
		if len(f) != 3 {
			// Push certificates and anything else unexpected.
			return nil, errors.New("unsupported push request")
		}
		if deny[f[2]] {
			return nil, fmt.Errorf("push to %s is not allowed through the credential gateway; push a branch and open a pull request", f[2])
		}
	}
	return nil, errors.New("push request too large")
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	deny := map[string]bool{}
	for _, ref := range strings.Split(env("DENY_PUSH_REFS", "refs/heads/main,refs/heads/master"), ",") {
		if ref = strings.TrimSpace(ref); ref != "" {
			deny[ref] = true
		}
	}
	g := &gateway{
		store: &bao{
			addr:    strings.TrimRight(env("BAO_ADDR", "http://openbao.credentials.svc.cluster.local:8200"), "/"),
			mount:   env("BAO_KV_MOUNT", "secret"),
			role:    env("BAO_ROLE", "cred-gateway"),
			jwtFile: env("BAO_JWT_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
			client:  &http.Client{Timeout: 10 * time.Second},
		},
		githubURL:    strings.TrimRight(env("GITHUB_URL", "https://github.com"), "/"),
		githubAPIURL: strings.TrimRight(env("GITHUB_API_URL", "https://api.github.com"), "/"),
		denyRefs:     deny,
		connectURL:   env("CONNECTIONS_URL", "/platform"),
		proxy:        http.DefaultTransport,
	}
	server := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: g, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	log.Printf("cred-gateway listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}
