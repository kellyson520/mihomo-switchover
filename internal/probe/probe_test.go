package probe

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

func TestExternalClientHonorsPerProbeTimeout(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	client, err := NewExternalClient(proxy.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result := client.Check(context.Background(), config.ProbeSpec{
		ID: "slow", URL: "http://target.example.test/slow", Timeout: 20 * time.Millisecond,
		ExpectedMin: 200, ExpectedMax: 499,
	})
	if result.Class != NetworkError {
		t.Fatalf("result=%+v", result)
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("probe timeout was ignored: %s", elapsed)
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

func TestRedactErrorRemovesQueryCredentials(t *testing.T) {
	err := &url.Error{
		Op:  "Get",
		URL: "https://api.example.test/v1/models?api_key=top-secret&region=us",
		Err: errors.New("connection refused"),
	}
	got := redactError(err)
	if strings.Contains(got, "top-secret") {
		t.Fatalf("redacted error leaked credential: %s", got)
	}
	if !strings.Contains(got, "api_key=%3Credacted%3E") {
		t.Fatalf("redacted error lost URL context: %s", got)
	}
}

func TestExternalClientAcceptsLoopbackSocks5Proxy(t *testing.T) {
	if _, err := NewExternalClient("socks5://127.0.0.1:17892", time.Second); err != nil {
		t.Fatalf("socks5 proxy rejected: %v", err)
	}
}

func TestExternalClientSendsRequestThroughSocks5hProxy(t *testing.T) {
	var proxyHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer target.Close()
	targetAddress := strings.TrimPrefix(target.URL, "http://")
	targetURL := strings.Replace(target.URL, "127.0.0.1", "target.invalid", 1)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		if err := socks5Handshake(conn); err != nil {
			return
		}
		atomic.AddInt32(&proxyHits, 1)
		upstream, dialErr := net.Dial("tcp", targetAddress)
		if dialErr != nil {
			return
		}
		defer upstream.Close()
		go func() { _, _ = io.Copy(upstream, conn) }()
		_, _ = io.Copy(conn, upstream)
	}()

	client, err := NewExternalClient("socks5h://"+listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result := client.Check(context.Background(), config.ProbeSpec{
		ID: "openai", URL: targetURL, Method: http.MethodGet,
		ExpectedMin: 200, ExpectedMax: 499,
	})
	if result.Class != ReachableHTTP || result.Status != http.StatusUnauthorized {
		t.Fatalf("result=%+v", result)
	}
	if atomic.LoadInt32(&proxyHits) != 1 {
		t.Fatalf("proxy_hits=%d", proxyHits)
	}
	<-done
}

func socks5Handshake(conn net.Conn) error {
	header := make([]byte, 3)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	if header[0] != 5 || header[2] != 0 {
		return io.ErrUnexpectedEOF
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return err
	}
	if request[0] != 5 || request[1] != 1 || request[3] != 3 {
		return io.ErrUnexpectedEOF
	}
	nameLength := make([]byte, 1)
	if _, err := io.ReadFull(conn, nameLength); err != nil {
		return err
	}
	nameAndPort := make([]byte, int(nameLength[0])+2)
	if _, err := io.ReadFull(conn, nameAndPort); err != nil {
		return err
	}
	_, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
	return err
}
