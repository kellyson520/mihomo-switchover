package mihomo

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClassifyMihomoErrorLog(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want LogHintCategory
	}{
		{name: "dial timeout", text: "[TCP] dial api.openai.com:443 error: i/o timeout", want: LogHintNetwork},
		{name: "tls failure", text: "[TLS] handshake failed: connection reset by peer", want: LogHintNetwork},
		{name: "provider filter", text: "[Provider] backup-channel pull error: doesn't match any proxy", want: LogHintNone},
		{name: "provider refresh", text: "[Provider] main-channel update error: timeout", want: LogHintNone},
		{name: "config error", text: "configuration field is invalid", want: LogHintNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyMihomoError(test.text); got != test.want {
				t.Fatalf("category=%q, want %q", got, test.want)
			}
		})
	}
}

func TestWatchErrorLogsReadsMihomoEnvelopeWithoutReturningRawMessage(t *testing.T) {
	const raw = "[TCP] dial api.openai.com:443 error: i/o timeout secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controller-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Connection") == "" {
			http.Error(w, "upgrade required", http.StatusBadRequest)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		frame := `{"type":"Log","payload":{"type":"error","payload":"` + raw + `"}}`
		_ = writeTestWebSocketFrame(conn, []byte(frame))
		time.Sleep(20 * time.Millisecond)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "controller-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan LogHint, 1)
	err = client.WatchErrorLogs(ctx, func(hint LogHint) {
		got <- hint
		cancel()
	})
	if err != nil && !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("watch error=%v", err)
	}
	select {
	case hint := <-got:
		if hint.Category != LogHintNetwork {
			t.Fatalf("hint=%+v", hint)
		}
		if strings.Contains(string(hint.Category), "secret-token") {
			t.Fatal("raw log message escaped through hint")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for log hint")
	}
}

func writeTestWebSocketFrame(writer io.Writer, payload []byte) error {
	header := []byte{0x81}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 65535:
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(len(payload)))
	default:
		header = append(header, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(len(payload)))
	}
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}
