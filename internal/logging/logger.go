package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type LoggerConfig struct {
	MaxBytes int
	Retain   int
}

type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	file  *os.File
	path  string
	cfg   LoggerConfig
	bytes int
}

func NewLogger(w io.Writer, cfg LoggerConfig) *Logger {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 10 * 1024 * 1024
	}
	if cfg.Retain <= 0 {
		cfg.Retain = 7
	}
	return &Logger{w: w, cfg: cfg}
}

func NewFileLogger(path string, cfg LoggerConfig) (*Logger, error) {
	if path == "" {
		return nil, fmt.Errorf("log path is required")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 10 * 1024 * 1024
	}
	if cfg.Retain <= 0 {
		cfg.Retain = 7
	}
	if err := os.MkdirAll(filepathDir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Logger{w: file, file: file, path: path, cfg: cfg, bytes: int(info.Size())}, nil
}

func (l *Logger) Event(event string, fields map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := make(map[string]any, len(fields)+2)
	entry["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	entry["event"] = event
	for key, value := range fields {
		entry[key] = redactValue(key, value)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if l.path != "" && l.bytes > 0 && l.bytes+len(data) > l.cfg.MaxBytes {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	if _, err := l.w.Write(data); err != nil {
		return err
	}
	l.bytes += len(data)
	return nil
}

func (l *Logger) rotateLocked() error {
	if l.file == nil || l.path == "" {
		return nil
	}
	if err := l.file.Close(); err != nil {
		return err
	}
	for index := l.cfg.Retain - 1; index >= 1; index-- {
		oldPath := fmt.Sprintf("%s.%d", l.path, index)
		newPath := fmt.Sprintf("%s.%d", l.path, index+1)
		if _, err := os.Stat(oldPath); err == nil {
			if err := os.Rename(oldPath, newPath); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return err
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	l.file = file
	l.w = file
	l.bytes = 0
	return nil
}

func filepathDir(path string) string {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		return "."
	}
	if index == 0 {
		return "/"
	}
	return path[:index]
}

func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	query := parsed.Query()
	for key := range query {
		query.Set(key, "<redacted>")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func redactValue(key string, value any) any {
	if sensitiveKey(key) {
		return "<redacted>"
	}
	switch typed := value.(type) {
	case string:
		if strings.HasPrefix(typed, "http://") || strings.HasPrefix(typed, "https://") {
			return RedactURL(typed)
		}
		return typed
	case map[string]any:
		result := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			result[childKey] = redactValue(childKey, childValue)
		}
		return result
	case []string:
		result := make([]string, len(typed))
		for i, item := range typed {
			result[i] = RedactURL(item)
		}
		return result
	default:
		return value
	}
}

func sensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	return strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") || lower == "key" || strings.HasSuffix(lower, "_key")
}
