package mihomo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrUnauthorized = errors.New("mihomo API unauthorized")

type Client struct {
	base   *url.URL
	secret string
	http   *http.Client
}

type Proxy struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Now  string   `json:"now"`
	All  []string `json:"all"`
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("mihomo API status %d: %s", e.Status, e.Body) }

func NewClient(rawBase, secret string, timeout time.Duration) (*Client, error) {
	base, err := url.Parse(rawBase)
	if err != nil || base.Scheme != "http" || !isLoopback(base.Hostname()) || base.Host == "" {
		return nil, errors.New("mihomo API must be an http loopback URL")
	}
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("mihomo API secret is required")
	}
	if timeout <= 0 {
		return nil, errors.New("mihomo API timeout must be positive")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: timeout}).DialContext}
	return &Client{base: base, secret: secret, http: &http.Client{Transport: transport, Timeout: timeout}}, nil
}

func (c *Client) Heartbeat(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrUnauthorized
	}
	if status < 200 || status >= 300 {
		return &apiError{Status: status, Body: trimBody(body)}
	}
	return nil
}

func (c *Client) GetProxy(ctx context.Context, name string) (Proxy, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/proxies/"+url.PathEscape(name), nil)
	if err != nil {
		return Proxy{}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return Proxy{}, ErrUnauthorized
	}
	if status < 200 || status >= 300 {
		return Proxy{}, &apiError{Status: status, Body: trimBody(body)}
	}
	var proxy Proxy
	if err := json.Unmarshal(body, &proxy); err != nil {
		return Proxy{}, fmt.Errorf("decode proxy %q: %w", name, err)
	}
	return proxy, nil
}

func (c *Client) SetProxy(ctx context.Context, group, node string) error {
	body, err := json.Marshal(map[string]string{"name": node})
	if err != nil {
		return err
	}
	status, raw, err := c.do(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), body)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrUnauthorized
	}
	if status < 200 || status >= 300 {
		return &apiError{Status: status, Body: trimBody(raw)}
	}
	return nil
}

func (c *Client) Delay(ctx context.Context, node, target string, timeout time.Duration) (int, error) {
	if timeout <= 0 {
		return 0, errors.New("delay timeout must be positive")
	}
	path := "/proxies/" + url.PathEscape(node) + "/delay?url=" + url.QueryEscape(target) + "&timeout=" + fmt.Sprint(timeout.Milliseconds())
	status, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return 0, ErrUnauthorized
	}
	if status < 200 || status >= 300 {
		return 0, &apiError{Status: status, Body: trimBody(body)}
	}
	var result struct {
		Delay int `json:"delay"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("decode delay for %q: %w", node, err)
	}
	return result.Delay, nil
}

func (c *Client) RefreshProvider(ctx context.Context, provider string) error {
	status, body, err := c.do(ctx, http.MethodPut, "/providers/proxies/"+url.PathEscape(provider), nil)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrUnauthorized
	}
	if status < 200 || status >= 300 {
		return &apiError{Status: status, Body: trimBody(body)}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	readBuf := make([]byte, 4096)
	for len(buf) < 1<<20 {
		n, readErr := resp.Body.Read(readBuf)
		buf = append(buf, readBuf[:n]...)
		if readErr != nil {
			break
		}
	}
	return resp.StatusCode, buf, nil
}

func trimBody(body []byte) string {
	if len(body) > 256 {
		body = body[:256]
	}
	return strings.TrimSpace(string(body))
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
