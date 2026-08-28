package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type State struct {
	CurrentChannel string                  `json:"current_channel"`
	FailureStreak  int                     `json:"failure_streak"`
	RecoveryStreak int                     `json:"recovery_streak"`
	HoldUntil      time.Time               `json:"hold_until,omitempty"`
	ForcedChannel  string                  `json:"forced_channel,omitempty"`
	ForceUntil     time.Time               `json:"force_until,omitempty"`
	ProviderLocks  map[string]ProviderLock `json:"provider_locks"`
	UpdatedAt      time.Time               `json:"updated_at"`
}

type ProviderLock struct {
	Provider       string    `json:"provider"`
	Group          string    `json:"group"`
	Node           string    `json:"node"`
	LastVerifiedAt time.Time `json:"last_verified_at,omitempty"`
	FailureStreak  int       `json:"failure_streak"`
}

func Default(channel string) State {
	if channel == "" {
		channel = "MAIN"
	}
	return State{CurrentChannel: channel, ProviderLocks: make(map[string]ProviderLock)}
}

type Store struct {
	path           string
	defaultChannel string
	mu             sync.Mutex
}

func NewStore(path, defaultChannel string) *Store {
	return &Store{path: path, defaultChannel: defaultChannel}
}

func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result State
	err := s.withFileLock(func() error {
		data, err := os.ReadFile(s.path)
		if errors.Is(err, os.ErrNotExist) {
			result = Default(s.defaultChannel)
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &result); err != nil {
			backup := s.path + ".corrupt." + time.Now().UTC().Format("20060102T150405.000000000Z")
			if renameErr := os.Rename(s.path, backup); renameErr != nil {
				return fmt.Errorf("decode state: %w; preserve corrupt state: %v", err, renameErr)
			}
			result = Default(s.defaultChannel)
		}
		return nil
	})
	if err != nil {
		return State{}, err
	}
	if result.CurrentChannel == "" {
		result.CurrentChannel = s.defaultChannel
	}
	if result.ProviderLocks == nil {
		result.ProviderLocks = make(map[string]ProviderLock)
	}
	return result, nil
}

func (s *Store) Save(value State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value.CurrentChannel == "" {
		value.CurrentChannel = s.defaultChannel
	}
	if value.ProviderLocks == nil {
		value.ProviderLocks = make(map[string]ProviderLock)
	}
	value.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return s.withFileLock(func() error {
		tmp := s.path + ".tmp"
		file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return err
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Rename(tmp, s.path)
	})
}

func (s *Store) withFileLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}
