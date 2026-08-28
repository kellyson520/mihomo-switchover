package mihomo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientReadsHeartbeatAndProxyGroup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"version":"Alpha"}`))
		case "/proxies/CHANNEL":
			_, _ = w.Write([]byte(`{"name":"CHANNEL","type":"Selector","now":"MAIN","all":["MAIN","BACKUP-USA"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	group, err := client.GetProxy(context.Background(), "CHANNEL")
	if err != nil {
		t.Fatal(err)
	}
	if group.Now != "MAIN" || len(group.All) != 2 {
		t.Fatalf("group=%+v", group)
	}
}

func TestClientSetsProxyAndEncodesNodePath(t *testing.T) {
	var gotMethod, setPath, delayPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			gotMethod, setPath = r.Method, r.URL.EscapedPath()
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotBody = body.Name
			w.WriteHeader(http.StatusNoContent)
			return
		}
		delayPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"delay":123}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetProxy(context.Background(), "MAIN", "香港 / 01"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Delay(context.Background(), "香港 / 01", "https://api.openai.com/v1/models", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || setPath != "/proxies/MAIN" || gotBody != "香港 / 01" {
		t.Fatalf("method=%s path=%s body=%q", gotMethod, setPath, gotBody)
	}
	if !strings.Contains(delayPath, "%2F") || strings.Contains(delayPath, " ") {
		t.Fatalf("node path was not safely encoded: %q", delayPath)
	}
}

func TestClientRejectsNonLoopbackController(t *testing.T) {
	if _, err := NewClient("http://example.com:9090", "secret", time.Second); err == nil {
		t.Fatal("expected non-loopback API to be rejected")
	}
}

func TestClientListsProxyGroupsWithoutUsingExternalProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxies" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"proxies":{"CHANNEL":{"name":"CHANNEL","type":"Selector","now":"MAIN","all":["MAIN","BACKUP-USA"]}}}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := client.ListProxies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if groups["CHANNEL"].Now != "MAIN" {
		t.Fatalf("groups=%+v", groups)
	}
}

func TestClientReadsProviderNodeHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/providers/proxies/backup-channel" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"name":"backup-channel","type":"Proxy","proxies":[{"name":"backup-old","alive":true,"history":[{"time":"2026-08-27T15:16:28.518485852Z","delay":753}]},{"name":"backup-dead","alive":false,"history":[{"time":"2026-08-27T15:16:27.765024872Z","delay":0}]}]}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := client.GetProvider(context.Background(), "backup-channel")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name != "backup-channel" || len(provider.Proxies) != 2 {
		t.Fatalf("provider=%+v", provider)
	}
	if !provider.Proxies[0].Alive || provider.Proxies[1].Alive {
		t.Fatalf("proxy health=%+v", provider.Proxies)
	}
	if provider.Proxies[0].History[0].Delay != 753 {
		t.Fatalf("history=%+v", provider.Proxies[0].History)
	}
}
