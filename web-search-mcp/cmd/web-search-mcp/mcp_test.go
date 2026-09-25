package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type headerRT struct{ h http.Header }

func (rt headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	for k, v := range rt.h {
		r.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPHandlerListsAndCallsTools(t *testing.T) {
	g := mustGuard(t, "")
	a := &app{cfg: config{fetchMaxChars: 100, fetchMaxBytes: 1 << 20}, guard: g, fetch: g.client(0)}
	srv := httptest.NewServer(a.handler())
	defer srv.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + "/",
		HTTPClient:           &http.Client{Transport: headerRT{http.Header{"X-User-Id": {"u1"}}}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "fetch_url,web_search" {
		t.Fatalf("tools %v", names)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "fetch_url", Arguments: map[string]any{"url": "http://169.254.169.254/latest/"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !res.IsError || !strings.Contains(text, "destination not allowed") || !strings.HasPrefix(text, "[UNTRUSTED WEB CONTENT") {
		t.Fatalf("metadata fetch not refused: %+v %q", res, text)
	}
}
