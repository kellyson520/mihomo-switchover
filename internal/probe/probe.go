package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mihomo-guardian/internal/config"
)

type ResultClass string

const (
	ReachableHTTP     ResultClass = "reachable_http"
	UpstreamHTTPError ResultClass = "upstream_http_error"
	UnexpectedHTTP    ResultClass = "unexpected_http"
	NetworkError      ResultClass = "network_error"
)

type Result struct {
	ProbeID  string
	URL      string
	Class    ResultClass
	Status   int
	Duration time.Duration
	Err      string
}

type ExternalClient struct {
	client *http.Client
}

func NewExternalClient(proxyRaw string, timeout time.Duration) (*ExternalClient, error) {
	if timeout <= 0 {
		return nil, errors.New("external probe timeout must be positive")
	}
	proxy, err := url.Parse(proxyRaw)
	if err != nil || proxy == nil || proxy.Hostname() == "" {
		return nil, errors.New("external probes require an http(s) or socks5 mihomo proxy")
	}
	if !isLoopback(proxy.Hostname()) {
		return nil, errors.New("external probe proxy must point to loopback")
	}
	if proxy.Port() == "" {
		return nil, errors.New("external probe proxy must include a port")
	}
	port, portErr := strconv.Atoi(proxy.Port())
	if portErr != nil || port < 1 || port > 65535 {
		return nil, errors.New("external probe proxy must include a valid port")
	}

	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: timeout,
	}
	switch strings.ToLower(proxy.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(proxy)
	case "socks5", "socks5h":
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialSOCKS5(ctx, proxy, timeout, network, address)
		}
	default:
		return nil, errors.New("external probes require an http(s) or socks5 mihomo proxy")
	}
	return &ExternalClient{client: &http.Client{Transport: transport, Timeout: timeout}}, nil
}

func dialSOCKS5(ctx context.Context, proxy *url.URL, timeout time.Duration, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("unsupported SOCKS5 network %q", network)
	}
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", proxy.Host)
	if err != nil {
		return nil, err
	}
	keepClosed := true
	defer func() {
		if keepClosed {
			_ = conn.Close()
		}
	}()

	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	if err := writeAll(conn, []byte{5, 1, 0}); err != nil {
		return nil, err
	}
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(conn, methodReply); err != nil {
		return nil, err
	}
	if methodReply[0] != 5 || methodReply[1] != 0 {
		return nil, errors.New("mihomo SOCKS5 proxy does not allow unauthenticated connections")
	}

	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return nil, fmt.Errorf("invalid SOCKS5 destination %q", address)
	}
	destinationPort, err := strconv.Atoi(portText)
	if err != nil || destinationPort < 1 || destinationPort > 65535 {
		return nil, fmt.Errorf("invalid SOCKS5 destination port %q", portText)
	}
	destination, err := socks5Destination(host, destinationPort)
	if err != nil {
		return nil, err
	}
	if err := writeAll(conn, destination); err != nil {
		return nil, err
	}

	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return nil, err
	}
	if reply[0] != 5 {
		return nil, errors.New("mihomo SOCKS5 proxy returned an invalid version")
	}
	if reply[1] != 0 {
		return nil, fmt.Errorf("mihomo SOCKS5 proxy rejected connection (code %d)", reply[1])
	}
	if err := discardSOCKS5Address(conn, reply[3]); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	keepClosed = false
	return conn, nil
}

func socks5Destination(host string, port int) ([]byte, error) {
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return append([]byte{5, 1, 0, 1}, append(ip4, byte(port>>8), byte(port))...), nil
		}
		ip16 := ip.To16()
		if ip16 != nil {
			return append([]byte{5, 1, 0, 4}, append(ip16, byte(port>>8), byte(port))...), nil
		}
	}
	if len(host) > 255 {
		return nil, errors.New("SOCKS5 destination hostname is too long")
	}
	request := []byte{5, 1, 0, 3, byte(len(host))}
	request = append(request, host...)
	request = append(request, byte(port>>8), byte(port))
	return request, nil
}

func discardSOCKS5Address(reader io.Reader, addressType byte) error {
	var addressLength int
	switch addressType {
	case 1:
		addressLength = net.IPv4len
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(reader, length[:]); err != nil {
			return err
		}
		addressLength = int(length[0])
	case 4:
		addressLength = net.IPv6len
	default:
		return fmt.Errorf("mihomo SOCKS5 proxy returned invalid address type %d", addressType)
	}
	address := make([]byte, addressLength+2)
	_, err := io.ReadFull(reader, address)
	return err
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func (c *ExternalClient) Check(ctx context.Context, spec config.ProbeSpec) Result {
	result, _ := c.Fetch(ctx, spec)
	return result
}

func (c *ExternalClient) Fetch(ctx context.Context, spec config.ProbeSpec) (Result, []byte) {
	started := time.Now()
	result := Result{ProbeID: spec.ID, URL: redactURL(spec.URL)}
	method := spec.Method
	if method == "" {
		method = http.MethodGet
	}
	requestContext := ctx
	cancel := func() {}
	if spec.Timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, spec.Timeout)
	}
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, method, spec.URL, nil)
	if err != nil {
		result.Class = NetworkError
		result.Err = err.Error()
		result.Duration = time.Since(started)
		return result, nil
	}
	req.Header.Set("User-Agent", "mihomo-guardian/1.0")
	resp, err := c.client.Do(req)
	result.Duration = time.Since(started)
	if err != nil {
		result.Class = classifyNetworkError(err)
		result.Err = redactError(err)
		return result, nil
	}
	defer resp.Body.Close()
	result.Status = resp.StatusCode
	result.Class = ClassifyStatus(resp.StatusCode, spec.ExpectedMin, spec.ExpectedMax)
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		result.Err = redactError(readErr)
	}
	return result, body
}

func ClassifyStatus(status, expectedMin, expectedMax int) ResultClass {
	if status == 0 {
		return NetworkError
	}
	if expectedMin == 0 {
		expectedMin = 200
	}
	if expectedMax == 0 {
		expectedMax = 499
	}
	if status >= 500 {
		return UpstreamHTTPError
	}
	if status >= expectedMin && status <= expectedMax && status >= 200 && status <= 499 {
		return ReachableHTTP
	}
	return UnexpectedHTTP
}

func classifyNetworkError(err error) ResultClass {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return NetworkError
	}
	return NetworkError
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	query := parsed.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "key") || strings.Contains(lower, "password") {
			query.Set(key, "<redacted>")
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func redactError(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Sprintf("%s %q: %s", urlErr.Op, redactURL(urlErr.URL), redactError(urlErr.Err))
	}
	return fmt.Sprintf("%s", err)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
