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

func TestClassifyBodyRejectsConfiguredGeminiLocationError(t *testing.T) {
	patterns := []string{`(?i)user\s+location.*not\s+supported`}
	if got := ClassifyBody(ReachableHTTP, []byte(`{"error":{"message":"User location is not supported for the API use."}}`), patterns); got != RoutePolicyError {
		t.Fatalf("location rejection class=%s, want %s", got, RoutePolicyError)
	}
	if got := ClassifyBody(ReachableHTTP, []byte(`{"error":{"message":"Method doesn't allow unregistered callers"}}`), patterns); got != ReachableHTTP {
		t.Fatalf("unregistered caller class=%s, want reachable HTTP", got)
	}
}

func TestExternalClientClassifiesGeminiLocationErrorWithoutLoggingBody(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"User location is not supported for the API use."}}`))
	}))
	defer proxy.Close()

	client, err := NewExternalClient(proxy.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result := client.Check(context.Background(), config.ProbeSpec{
		ID: "gemini", URL: "http://generativelanguage.googleapis.com/v1beta/models",
		ExpectedMin: 200, ExpectedMax: 499,
		RejectBodyPatterns: []string{`(?i)user\s+location.*not\s+supported`},
	})
	if result.Class != RoutePolicyError || result.Status != http.StatusBadRequest {
		t.Fatalf("result=%+v, want route policy error with HTTP 400", result)
	}
	if strings.Contains(strings.ToLower(result.Err), "user location") || strings.Contains(strings.ToLower(result.Err), "not supported") {
		t.Fatalf("response body leaked into operator error: %q", result.Err)
	}
}

func TestExternalClientTreatsResponseBodyReadFailureAsNetworkFailure(t *testing.T) {
	client := &ExternalClient{client: &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       failingBodyReader{},
			}, nil
		}),
	}}

	result, _ := client.Fetch(context.Background(), config.ProbeSpec{
		ID: "body-read", URL: "https://api.example.test/models",
		ExpectedMin: 200, ExpectedMax: 499,
	})
	if result.Class != NetworkError || result.ErrorKind != ErrorKindBodyRead {
		t.Fatalf("result=%+v, want network body-read failure", result)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type failingBodyReader struct{}

func (failingBodyReader) Read([]byte) (int, error) { return 0, errors.New("body read failed") }

func (failingBodyReader) Close() error { return nil }

func TestClassifyErrorKindPreservesTypedNetworkCategories(t *testing.T) {
	if got := classifyErrorKind(context.DeadlineExceeded); got != ErrorKindTimeout {
		t.Fatalf("deadline kind=%s, want timeout", got)
	}
	dns := &net.DNSError{Err: "no such host", Name: "example.invalid"}
	if got := classifyErrorKind(dns); got != ErrorKindDNS {
		t.Fatalf("dns kind=%s, want dns", got)
	}
	wrapped := &TransportError{Kind: ErrorKindTCP, Err: dns}
	if !errors.Is(wrapped, dns) {
		t.Fatal("TransportError must preserve errors.Is behavior")
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
