package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
)

func TestExternalClientSendsRequestThroughMihomoProxy(t *testing.T) {
	var targetHits, proxyHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer proxy.Close()

	client, err := NewExternalClient(proxy.URL, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result := client.Check(context.Background(), config.ProbeSpec{ID: "openai", URL: target.URL, Method: "GET", ExpectedMin: 200, ExpectedMax: 499})
	if result.Class != ReachableHTTP || result.Status != http.StatusUnauthorized {
		t.Fatalf("result=%+v", result)
	}
	if atomic.LoadInt32(&proxyHits) != 1 || atomic.LoadInt32(&targetHits) != 0 {
		t.Fatalf("proxy_hits=%d target_hits=%d", proxyHits, targetHits)
	}
}

func TestExternalClientRejectsDirectMode(t *testing.T) {
	if _, err := NewExternalClient("", time.Second); err == nil {
		t.Fatal("expected direct external access to be rejected")
	}
}

func TestClassifyStatusAndNetworkErrors(t *testing.T) {
	if got := ClassifyStatus(401, 200, 499); got != ReachableHTTP {
		t.Fatalf("401 class=%s", got)
	}
	if got := ClassifyStatus(503, 200, 499); got != UpstreamHTTPError {
		t.Fatalf("503 class=%s", got)
	}
	if got := ClassifyStatus(0, 200, 499); got != NetworkError {
		t.Fatalf("0 class=%s", got)
	}
}
