package mihomo

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LogHintCategory is the small, redacted signal passed from mihomo's log
// stream to the runtime decision loop. Raw mihomo log messages never cross
// this boundary.
type LogHintCategory string

const (
	LogHintNone    LogHintCategory = ""
	LogHintNetwork LogHintCategory = "network"
)

type LogHint struct {
	Category LogHintCategory
}

const (
	webSocketGUID      = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxLogFramePayload = 1 << 20
)

// WatchErrorLogs subscribes to mihomo's local error log stream. A temporary
// connection failure is retried with bounded backoff; only context
// cancellation terminates the watcher. This is intentionally best-effort and
// cannot affect the mihomo process or the guardian decision loop.
func (c *Client) WatchErrorLogs(ctx context.Context, callback func(LogHint)) error {
	if callback == nil {
		return errors.New("mihomo log callback is required")
	}
	backoff := time.Second
	for {
		err := c.watchErrorLogsOnce(ctx, callback)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			backoff = time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) watchErrorLogsOnce(ctx context.Context, callback func(LogHint)) error {
	dialer := &net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", c.base.Host)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	requestKey, err := writeWebSocketHandshake(conn, c.base, c.secret)
	if err != nil {
		return err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	reader := bufio.NewReader(conn)
	request := &http.Request{Method: http.MethodGet}
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return err
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("mihomo log websocket status %d", response.StatusCode)
	}
	if !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
		return errors.New("mihomo log websocket upgrade was not accepted")
	}
	accept := response.Header.Get("Sec-WebSocket-Accept")
	if accept == "" {
		return errors.New("mihomo log websocket accept key is missing")
	}
	if accept != websocketAccept(requestKey) {
		return errors.New("mihomo log websocket accept key is invalid")
	}

	for {
		opcode, payload, err := readWebSocketFrame(reader)
		if err != nil {
			return err
		}
		switch opcode {
		case 0x1: // text
			if hint, ok := parseMihomoLogHint(payload); ok {
				callback(hint)
			}
		case 0x8: // close
			return nil
		case 0x9: // ping
			if err := writeWebSocketFrame(conn, 0xA, payload); err != nil {
				return err
			}
		case 0xA: // pong
			continue
		default:
			// Ignore binary and continuation frames. Mihomo emits complete
			// text messages for the log endpoint.
			continue
		}
	}
}

func writeWebSocketHandshake(conn net.Conn, base *url.URL, secret string) (string, error) {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := "/logs"
	query := base.Query()
	query.Set("level", "error")
	path += "?" + query.Encode()
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", path, base.Host, secret, key)
	_, err := io.WriteString(conn, request)
	return key, err
}

func websocketAccept(key string) string {
	hash := sha1.Sum([]byte(key + webSocketGUID))
	return base64.StdEncoding.EncodeToString(hash[:])
}

func readWebSocketFrame(reader *bufio.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	if header[0]&0x70 != 0 {
		return 0, nil, errors.New("mihomo log websocket reserved bits are set")
	}
	opcode := header[0] & 0x0F
	length := uint64(header[1] & 0x7F)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if length > maxLogFramePayload {
		return 0, nil, errors.New("mihomo log websocket payload is too large")
	}
	masked := header[1]&0x80 != 0
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
	}
	return opcode, payload, nil
}

func writeWebSocketFrame(conn net.Conn, opcode byte, payload []byte) error {
	if len(payload) > maxLogFramePayload {
		return errors.New("mihomo log websocket payload is too large")
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header := []byte{0x80 | opcode}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload))|0x80)
	case len(payload) <= 65535:
		header = append(header, 126|0x80, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(len(payload)))
	default:
		header = append(header, 127|0x80, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(len(payload)))
	}
	maskedPayload := append([]byte(nil), payload...)
	for index := range maskedPayload {
		maskedPayload[index] ^= mask[index%4]
	}
	if _, err := conn.Write(append(header, mask...)); err != nil {
		return err
	}
	_, err := conn.Write(maskedPayload)
	return err
}

func parseMihomoLogHint(raw []byte) (LogHint, bool) {
	var envelope struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || !strings.EqualFold(envelope.Type, "log") {
		return LogHint{}, false
	}
	var payload struct {
		Type    string `json:"type"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil || !strings.EqualFold(payload.Type, "error") {
		return LogHint{}, false
	}
	category := classifyMihomoError(payload.Payload)
	if category == LogHintNone {
		return LogHint{}, false
	}
	return LogHint{Category: category}, true
}

func classifyMihomoError(message string) LogHintCategory {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" || strings.Contains(lower, "[provider]") || strings.Contains(lower, "provider") {
		return LogHintNone
	}
	for _, token := range []string{
		"dial", "connection", "tls", "timeout", "timed out", "reset by peer",
		"broken pipe", "no route", "network is unreachable", "connection refused",
	} {
		if strings.Contains(lower, token) {
			return LogHintNetwork
		}
	}
	return LogHintNone
}
