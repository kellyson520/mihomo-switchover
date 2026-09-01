package quality

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"mihomo-guardian/internal/config"
)

const (
	nodesDirectory                = "nodes"
	historyDirectory              = "history"
	stabilityFile                 = "stability.json"
	stabilityHistoryFile          = "stability-history.jsonl"
	recommendationsFile           = "recommendations.json"
	recommendationsLockFile       = "recommendations.lock"
	scanRunLockFile               = "scan-run.lock"
	scanProgressFile              = "scan-progress.json"
	scanLockFile                  = "scan.lock"
	storageFileMode               = 0600
	storageDirectoryMode          = 0700
	storageTimestampLayout        = "20060102T150405.000000000Z"
	storageCorruptTimestampLayout = "20060102T150405.000000000Z"
)

var safeTargetPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
var sensitiveAssignmentPattern = regexp.MustCompile(`(?i)(token|secret|password|api[_-]?key)=([^&\s,;)]+)`)

// Store is the persistent quality data store. It never reads or writes the
// realtime guardian state.json; that state belongs to internal/state.Store.
type Store struct {
	root    string
	mu      chan struct{}
	initErr error
}

// NewStore creates a store rooted at root and prepares its persistent layout.
// Because this constructor follows the existing project's pointer-only store
// convention, an initialization failure is returned by the first operation.
func NewStore(root string) *Store {
	store := &Store{root: filepath.Clean(strings.TrimSpace(root)), mu: make(chan struct{}, 1)}
	if store.root == "" || store.root == "." {
		store.initErr = ErrInvalidStore
		return store
	}
	store.initErr = store.ensureLayout()
	return store
}

// OpenStore is an error-returning constructor for callers that want to fail
// during initialization instead of at the first store operation.
func OpenStore(root string) (*Store, error) {
	store := NewStore(root)
	if store.initErr != nil {
		return nil, store.initErr
	}
	return store, nil
}

func (s *Store) Root() string       { return s.root }
func (s *Store) NodesDir() string   { return filepath.Join(s.root, nodesDirectory) }
func (s *Store) HistoryDir() string { return filepath.Join(s.root, historyDirectory) }
func (s *Store) StabilityPath() string {
	return filepath.Join(s.root, stabilityFile)
}
func (s *Store) StabilityHistoryPath() string {
	return filepath.Join(s.root, stabilityHistoryFile)
}
func (s *Store) RecommendationsPath() string {
	return filepath.Join(s.root, recommendationsFile)
}
func (s *Store) RecommendationsLockPath() string {
	return filepath.Join(s.root, recommendationsLockFile)
}
func (s *Store) ScanProgressPath() string { return filepath.Join(s.root, scanProgressFile) }
func (s *Store) ScanLockPath() string     { return filepath.Join(s.root, scanLockFile) }
func (s *Store) ScanRunLockPath() string  { return filepath.Join(s.root, scanRunLockFile) }
func (s *Store) LatestPath(target string) string {
	return s.latestPath(target)
}
func (s *Store) NodePath(key NodeKey) string {
	return s.nodePath(key)
}

func (s *Store) latestPath(target string) string {
	return filepath.Join(s.root, "latest-"+safeTargetName(target)+".json")
}

func (s *Store) nodePath(key NodeKey) string {
	return filepath.Join(s.NodesDir(), key.ID()+".json")
}

func (s *Store) lockPath() string { return filepath.Join(s.root, scanLockFile) }

func (s *Store) withLock(fn func() error) error {
	if s == nil || s.root == "" || s.root == "." {
		return ErrInvalidStore
	}
	if s.initErr != nil {
		return s.initErr
	}
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	if err := s.ensureLayout(); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, storageFileMode)
	if err != nil {
		return err
	}
	if err := lock.Chmod(storageFileMode); err != nil {
		_ = lock.Close()
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

// withScanRunLock protects the whole select -> collect -> persist lifecycle
// of a full quality scan. It uses a separate lock from per-file store writes,
// so SaveReport can still take its normal store lock while the scan is held.
// Non-blocking acquisition makes a manual run fail closed instead of waiting
// behind a daemon that may be probing a node for several minutes.
func (s *Store) withScanRunLock(fn func() error) error {
	if s == nil || s.root == "" || s.root == "." {
		return ErrInvalidStore
	}
	if s.initErr != nil {
		return s.initErr
	}
	if err := s.ensureLayout(); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.ScanRunLockPath(), os.O_CREATE|os.O_RDWR, storageFileMode)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lock.Chmod(storageFileMode); err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrScanBusy
		}
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

// WithRecommendationsLock serializes the read/derive/write transaction used
// to rebuild recommendations. It is separate from the scan lifecycle lock so
// the hourly read-only summary can update recommendations while a full scan
// is collecting evidence, without allowing stale snapshots to overwrite one
// another across processes.
func (s *Store) WithRecommendationsLock(fn func() error) error {
	if s == nil || s.root == "" || s.root == "." {
		return ErrInvalidStore
	}
	if s.initErr != nil {
		return s.initErr
	}
	if err := s.ensureLayout(); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.RecommendationsLockPath(), os.O_CREATE|os.O_RDWR, storageFileMode)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lock.Chmod(storageFileMode); err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *Store) ensureLayout() error {
	if err := os.MkdirAll(s.root, storageDirectoryMode); err != nil {
		return err
	}
	if err := os.Chmod(s.root, storageDirectoryMode); err != nil {
		return err
	}
	for _, directory := range []string{s.NodesDir(), s.HistoryDir()} {
		if err := os.MkdirAll(directory, storageDirectoryMode); err != nil {
			return err
		}
		if err := os.Chmod(directory, storageDirectoryMode); err != nil {
			return err
		}
	}
	lock, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, storageFileMode)
	if err != nil {
		return err
	}
	if err := lock.Chmod(storageFileMode); err != nil {
		_ = lock.Close()
		return err
	}
	return lock.Close()
}

// SaveReport persists one report, updates its identity record and the target
// latest pointer, and keeps an immutable copy in history. All three files are
// individually atomically replaced under the process lock.
func (s *Store) SaveReport(report Report) (NodeRecord, error) {
	if err := report.Validate(); err != nil {
		return NodeRecord{}, err
	}
	report = prepareReport(report)
	var result NodeRecord
	err := s.withLock(func() error {
		key := report.Identity
		var record NodeRecord
		exists, err := readJSON(s.nodePath(key), &record)
		if err != nil {
			return err
		}
		if !exists || record.Identity.ID() != key.ID() {
			record = NodeRecord{Identity: key, CreatedAt: report.ObservedAt}
		} else {
			record.Identity = key
			if record.CreatedAt.IsZero() {
				record.CreatedAt = report.ObservedAt
			}
		}
		if record.Baseline == nil && report.BaselineEligible() {
			record.Baseline = &Baseline{
				Identity:          key,
				Score:             report.EffectiveScore,
				QualityScore:      report.QualityScore,
				StabilityScore:    report.StabilityScore,
				ConfidencePercent: report.ConfidencePercent,
				CreatedAt:         report.ObservedAt,
				ObservedAt:        report.ObservedAt,
			}
		}
		if record.Best == nil || report.EffectiveScore > record.BestScore {
			best := report
			record.Best = &best
			record.BestScore = report.EffectiveScore
		}
		latest := report
		record.Latest = &latest
		if report.Eligible {
			lastGood := report
			record.LastGood = &lastGood
		}
		record.ReportCount++
		record.UpdatedAt = report.ObservedAt

		if err := writeJSONAtomic(s.nodePath(key), record); err != nil {
			return err
		}
		historyPath, err := s.nextHistoryPath(report)
		if err != nil {
			return err
		}
		if err := writeJSONAtomic(historyPath, report); err != nil {
			return err
		}
		if err := writeJSONAtomic(s.latestPath(key.Target), report); err != nil {
			return err
		}
		result = record
		return nil
	})
	return result, err
}

func prepareReport(report Report) Report {
	report.Identity = report.Identity.Canonical()
	report.ObservedAt = report.ObservedAt.UTC()
	if report.ObservedAt.IsZero() {
		report.ObservedAt = time.Now().UTC()
	}
	if report.ProviderAlive {
		report.Provider.Alive = true
	}
	if report.ProviderHistoryFresh {
		report.Provider.HistoryFresh = true
	}
	if report.ProviderHistorySamples > 0 {
		report.Provider.HistorySamples = report.ProviderHistorySamples
	}
	if !report.ProviderLastSampleAt.IsZero() {
		report.Provider.LastSampleAt = report.ProviderLastSampleAt
	}
	if report.ProviderAlive || report.Provider.Alive {
		report.ProviderAlive = true
	}
	if report.ProviderHistoryFresh || report.Provider.HistoryFresh {
		report.ProviderHistoryFresh = true
	}
	if report.ProviderHistorySamples == 0 {
		report.ProviderHistorySamples = report.Provider.HistorySamples
	}
	if report.ProviderLastSampleAt.IsZero() {
		report.ProviderLastSampleAt = report.Provider.LastSampleAt
	}
	return report
}

func (s *Store) nextHistoryPath(report Report) (string, error) {
	base := report.ObservedAt.UTC().Format(storageTimestampLayout) + "-" + report.Identity.ID()
	path := filepath.Join(s.HistoryDir(), base+".json")
	for index := 1; ; index++ {
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		path = filepath.Join(s.HistoryDir(), fmt.Sprintf("%s-%d.json", base, index))
	}
}

func (s *Store) LoadNode(key NodeKey) (NodeRecord, error) {
	if err := key.Validate(); err != nil {
		return NodeRecord{}, err
	}
	var record NodeRecord
	err := s.withLock(func() error {
		exists, err := readJSON(s.nodePath(key.Canonical()), &record)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return nil
	})
	return record, err
}

func (s *Store) ListNodeRecords() ([]NodeRecord, error) {
	var records []NodeRecord
	err := s.withLock(func() error {
		entries, err := os.ReadDir(s.NodesDir())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			var record NodeRecord
			exists, err := readJSON(filepath.Join(s.NodesDir(), entry.Name()), &record)
			if err != nil {
				return err
			}
			if exists {
				records = append(records, record)
			}
		}
		sort.Slice(records, func(i, j int) bool { return records[i].Identity.ID() < records[j].Identity.ID() })
		return nil
	})
	return records, err
}

func (s *Store) LoadLatest(target string) (Report, error) {
	var report Report
	err := s.withLock(func() error {
		exists, err := readJSON(s.latestPath(target), &report)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return nil
	})
	return report, err
}

func (s *Store) LoadLatestTarget(target string) (Report, error) { return s.LoadLatest(target) }

func (s *Store) ListReports(key NodeKey) ([]Report, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	var reports []Report
	err := s.withLock(func() error {
		entries, err := os.ReadDir(s.HistoryDir())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			var report Report
			exists, err := readJSON(filepath.Join(s.HistoryDir(), entry.Name()), &report)
			if err != nil {
				return err
			}
			if exists && report.Identity.ID() == key.ID() {
				reports = append(reports, report)
			}
		}
		sort.Slice(reports, func(i, j int) bool { return reports[i].ObservedAt.Before(reports[j].ObservedAt) })
		return nil
	})
	return reports, err
}

func (s *Store) SaveStability(snapshot StabilitySnapshot) error {
	var err error
	snapshot, err = normalizeStabilitySnapshot(snapshot)
	if err != nil {
		return err
	}
	return s.withLock(func() error {
		return s.saveStabilityLocked(snapshot)
	})
}

// ApplyStability persists a snapshot and refreshes only an already-known
// node's latest report. It never creates a NodeRecord, which prevents an
// hourly history summary from inventing an IP identity or baseline.
func (s *Store) ApplyStability(key NodeKey, snapshot StabilitySnapshot, providerAlive bool, minimumConfidence int) (NodeRecord, error) {
	if err := key.Validate(); err != nil {
		return NodeRecord{}, err
	}
	key = key.Canonical()
	var err error
	snapshot, err = normalizeStabilitySnapshot(snapshot)
	if err != nil {
		return NodeRecord{}, err
	}
	if snapshot.Identity.ID() != key.ID() {
		return NodeRecord{}, fmt.Errorf("%w: stability identity does not match node identity", ErrInvalidReport)
	}
	if minimumConfidence <= 0 {
		minimumConfidence = 70
	}
	var result NodeRecord
	err = s.withLock(func() error {
		var record NodeRecord
		exists, err := readJSON(s.nodePath(key), &record)
		if err != nil {
			return err
		}
		if !exists || record.Identity.ID() != key.ID() || record.Latest == nil {
			return ErrNotFound
		}

		latest := *record.Latest
		latest.Identity = key
		latest.ProviderAlive = providerAlive
		latest.Provider.Alive = providerAlive
		latest.Provider.CheckedAt = snapshot.ObservedAt
		latest.ProviderHistoryFresh = snapshot.Fresh
		latest.Provider.HistoryFresh = snapshot.Fresh
		latest.ProviderHistorySamples = snapshot.Samples
		latest.Provider.HistorySamples = snapshot.Samples
		latest.ProviderLastSampleAt = snapshot.LastSampleAt
		latest.Provider.LastSampleAt = snapshot.LastSampleAt
		latest.StabilityScore = clampScore(snapshot.StabilityScore)
		latest.StabilityObservedAt = snapshot.ObservedAt

		quality := ScoreQuality(latest.VendorResults, latest.SourceEvidence, latest.RiskEvidence)
		latest.QualityScore = quality.Score
		latest.ConfidencePercent = quality.Confidence
		latest.Complete = quality.Complete && providerAlive && snapshot.Known && snapshot.Fresh
		latest.Eligible = latest.Complete && latest.ConfidencePercent >= minimumConfidence
		latest.EffectiveScore = EffectiveScore(latest.QualityScore, latest.StabilityScore)
		latest.Errors = stabilityErrors(latest.Errors, providerAlive, snapshot, latest.Identity.Provider)

		record.Latest = &latest
		if record.Best == nil || latest.EffectiveScore > record.BestScore {
			best := latest
			record.Best = &best
			record.BestScore = latest.EffectiveScore
		}
		if latest.Eligible {
			lastGood := latest
			record.LastGood = &lastGood
		}
		record.UpdatedAt = snapshot.ObservedAt
		if err := writeJSONAtomic(s.nodePath(key), record); err != nil {
			return err
		}
		if err := s.saveStabilityLocked(snapshot); err != nil {
			return err
		}

		var targetLatest Report
		latestExists, err := readJSON(s.latestPath(key.Target), &targetLatest)
		if err != nil {
			return err
		}
		if latestExists && targetLatest.Identity.ID() == key.ID() {
			if err := writeJSONAtomic(s.latestPath(key.Target), latest); err != nil {
				return err
			}
		}
		result = record
		return nil
	})
	return result, err
}

func normalizeStabilitySnapshot(snapshot StabilitySnapshot) (StabilitySnapshot, error) {
	if err := snapshot.Identity.Validate(); err != nil {
		return StabilitySnapshot{}, err
	}
	snapshot.Identity = snapshot.Identity.Canonical()
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = time.Now().UTC()
	} else {
		snapshot.ObservedAt = snapshot.ObservedAt.UTC()
	}
	return snapshot, nil
}

func (s *Store) saveStabilityLocked(snapshot StabilitySnapshot) error {
	snapshots, err := s.readStabilityMap()
	if err != nil {
		return err
	}
	snapshots[snapshot.Identity.ID()] = snapshot
	if err := writeJSONAtomic(filepath.Join(s.root, stabilityFile), snapshots); err != nil {
		return err
	}
	return s.appendStabilityHistory(snapshot)
}

func stabilityErrors(existing []ReportError, providerAlive bool, snapshot StabilitySnapshot, provider string) []ReportError {
	result := make([]ReportError, 0, len(existing)+2)
	for _, item := range existing {
		if item.Code == ErrorProviderUnhealthy || item.Code == ErrorStabilityUnknown {
			continue
		}
		result = append(result, item)
	}
	if !providerAlive {
		result = append(result, ReportError{Code: ErrorProviderUnhealthy, Source: provider, Message: "mihomo provider node is not alive", ObservedAt: snapshot.ObservedAt})
	}
	if !snapshot.Known || !snapshot.Fresh {
		result = append(result, ReportError{Code: ErrorStabilityUnknown, Source: provider, Message: "mihomo history is missing, stale, or insufficient", ObservedAt: snapshot.ObservedAt})
	}
	return result
}

func (s *Store) LoadStability() (map[string]StabilitySnapshot, error) {
	var snapshots map[string]StabilitySnapshot
	err := s.withLock(func() error {
		var err error
		snapshots, err = s.readStabilityMap()
		return err
	})
	return snapshots, err
}

func (s *Store) LoadStabilitySnapshot(key NodeKey) (StabilitySnapshot, error) {
	snapshots, err := s.LoadStability()
	if err != nil {
		return StabilitySnapshot{}, err
	}
	snapshot, ok := snapshots[key.ID()]
	if !ok {
		return StabilitySnapshot{}, ErrNotFound
	}
	return snapshot, nil
}

func (s *Store) LoadStabilityHistory() ([]StabilitySnapshot, error) {
	var snapshots []StabilitySnapshot
	err := s.withLock(func() error {
		var err error
		snapshots, err = s.readStabilityHistory()
		return err
	})
	return snapshots, err
}

func (s *Store) SaveRecommendations(recommendations []Recommendation) error {
	copyOf := append([]Recommendation(nil), recommendations...)
	return s.withLock(func() error {
		return writeJSONAtomic(filepath.Join(s.root, recommendationsFile), copyOf)
	})
}

func (s *Store) SaveRecommendation(recommendation Recommendation) error {
	return s.withLock(func() error {
		recommendations, err := s.readRecommendations()
		if err != nil {
			return err
		}
		key := recommendationKey(recommendation)
		replaced := false
		for index := range recommendations {
			if recommendationKey(recommendations[index]) == key {
				recommendations[index] = recommendation
				replaced = true
				break
			}
		}
		if !replaced {
			recommendations = append(recommendations, recommendation)
		}
		return writeJSONAtomic(filepath.Join(s.root, recommendationsFile), recommendations)
	})
}

func (s *Store) LoadRecommendations() ([]Recommendation, error) {
	var recommendations []Recommendation
	err := s.withLock(func() error {
		var err error
		recommendations, err = s.readRecommendations()
		return err
	})
	return recommendations, err
}

func (s *Store) SaveScanProgress(progress ScanProgress) error {
	if progress.UpdatedAt.IsZero() {
		progress.UpdatedAt = time.Now().UTC()
	}
	return s.withLock(func() error {
		return writeJSONAtomic(filepath.Join(s.root, scanProgressFile), progress)
	})
}

func (s *Store) SaveProgress(progress ScanProgress) error { return s.SaveScanProgress(progress) }

func (s *Store) LoadScanProgress() (ScanProgress, error) {
	var progress ScanProgress
	err := s.withLock(func() error {
		exists, err := readJSON(filepath.Join(s.root, scanProgressFile), &progress)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		return nil
	})
	return progress, err
}

func (s *Store) LoadProgress() (ScanProgress, error) { return s.LoadScanProgress() }

// Prune applies report-count and age retention to immutable report history,
// and removes old stability-history lines. Current node records, latest
// pointers, baselines, recommendations and progress are never pruned here.
func (s *Store) Prune(retention config.QualityRetentionConfig, now time.Time) error {
	if retention.Reports < 1 && retention.HistoryDays < 1 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := time.Time{}
	if retention.HistoryDays > 0 {
		cutoff = now.Add(-time.Duration(retention.HistoryDays) * 24 * time.Hour)
	}
	return s.withLock(func() error {
		if err := s.pruneReportHistory(cutoff, retention.Reports); err != nil {
			return err
		}
		return s.pruneStabilityHistory(cutoff)
	})
}

func (s *Store) ApplyRetention(retention config.QualityRetentionConfig, now time.Time) error {
	return s.Prune(retention, now)
}

func (s *Store) pruneReportHistory(cutoff time.Time, maxReports int) error {
	entries, err := os.ReadDir(s.HistoryDir())
	if err != nil {
		return err
	}
	type item struct {
		path string
		when time.Time
	}
	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		when, ok := historyTime(entry.Name())
		if !ok {
			continue
		}
		items = append(items, item{path: filepath.Join(s.HistoryDir(), entry.Name()), when: when})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].when.After(items[j].when) })
	kept := 0
	for _, current := range items {
		withinAge := cutoff.IsZero() || !current.when.Before(cutoff)
		withinCount := maxReports <= 0 || kept < maxReports
		if withinAge && withinCount {
			kept++
			continue
		}
		if err := os.Remove(current.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) pruneStabilityHistory(cutoff time.Time) error {
	path := filepath.Join(s.root, stabilityHistoryFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lines, valid := parseStabilityLines(data)
	if !valid {
		if _, err := preserveCorrupt(path); err != nil {
			return err
		}
		return nil
	}
	if cutoff.IsZero() {
		return nil
	}
	var builder strings.Builder
	for _, snapshot := range lines {
		if snapshot.ObservedAt.IsZero() || !snapshot.ObservedAt.Before(cutoff) {
			encoded, err := json.Marshal(sanitizeSnapshot(snapshot))
			if err != nil {
				return err
			}
			builder.Write(encoded)
			builder.WriteByte('\n')
		}
	}
	return writeBytesAtomic(path, []byte(builder.String()))
}

func (s *Store) readStabilityMap() (map[string]StabilitySnapshot, error) {
	path := filepath.Join(s.root, stabilityFile)
	var snapshots map[string]StabilitySnapshot
	exists, err := readJSON(path, &snapshots)
	if err != nil {
		return nil, err
	}
	if !exists || snapshots == nil {
		snapshots = make(map[string]StabilitySnapshot)
	}
	return snapshots, nil
}

func (s *Store) appendStabilityHistory(snapshot StabilitySnapshot) error {
	path := filepath.Join(s.root, stabilityHistoryFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = nil
	} else if err != nil {
		return err
	}
	if _, valid := parseStabilityLines(data); !valid {
		if _, err := preserveCorrupt(path); err != nil {
			return err
		}
		data = nil
	}
	encoded, err := json.Marshal(sanitizeSnapshot(snapshot))
	if err != nil {
		return err
	}
	data = append(data, encoded...)
	data = append(data, '\n')
	return writeBytesAtomic(path, data)
}

func (s *Store) readStabilityHistory() ([]StabilitySnapshot, error) {
	data, err := os.ReadFile(filepath.Join(s.root, stabilityHistoryFile))
	if errors.Is(err, os.ErrNotExist) {
		return []StabilitySnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	lines, valid := parseStabilityLines(data)
	if !valid {
		if _, err := preserveCorrupt(filepath.Join(s.root, stabilityHistoryFile)); err != nil {
			return nil, err
		}
		return []StabilitySnapshot{}, nil
	}
	return lines, nil
}

func readRecommendationsForStore(s *Store) ([]Recommendation, error) {
	var recommendations []Recommendation
	exists, err := readJSON(filepath.Join(s.root, recommendationsFile), &recommendations)
	if err != nil {
		return nil, err
	}
	if !exists || recommendations == nil {
		return []Recommendation{}, nil
	}
	return recommendations, nil
}

func readJSON(path string, target any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		_, preserveErr := preserveCorrupt(path)
		if preserveErr != nil {
			return false, &CorruptFileError{Path: path, Cause: fmt.Errorf("decode: %w; preserve: %v", err, preserveErr)}
		}
		return false, nil
	}
	return true, nil
}

func preserveCorrupt(path string) (string, error) {
	if err := os.Chmod(path, storageFileMode); err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format(storageCorruptTimestampLayout)
	backup := path + ".corrupt-" + stamp
	for index := 1; ; index++ {
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return "", err
		}
		backup = fmt.Sprintf("%s.corrupt-%s-%d", path, stamp, index)
	}
	if err := os.Rename(path, backup); err != nil {
		return "", err
	}
	return backup, nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(sanitizeValue(value), "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeBytesAtomic(path, data)
}

func writeBytesAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, storageDirectoryMode); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(storageFileMode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryFile.Close()
	if err := directoryFile.Sync(); err != nil {
		return err
	}
	return nil
}

func safeTargetName(target string) string {
	target = strings.TrimSpace(target)
	if safeTargetPattern.MatchString(target) {
		return target
	}
	digest := sha256.Sum256([]byte(target))
	return hex.EncodeToString(digest[:])
}

func historyTime(name string) (time.Time, bool) {
	prefix := strings.SplitN(name, "-", 2)[0]
	when, err := time.Parse(storageTimestampLayout, prefix)
	return when, err == nil
}

func parseStabilityLines(data []byte) ([]StabilitySnapshot, bool) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return []StabilitySnapshot{}, true
	}
	var result []StabilitySnapshot
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var snapshot StabilitySnapshot
		if err := json.Unmarshal([]byte(line), &snapshot); err != nil {
			return nil, false
		}
		result = append(result, snapshot)
	}
	if err := scanner.Err(); err != nil {
		return nil, false
	}
	return result, true
}

func sanitizeValue(value any) any {
	switch typed := value.(type) {
	case Report:
		return sanitizeReport(typed)
	case *Report:
		if typed == nil {
			return (*Report)(nil)
		}
		clean := sanitizeReport(*typed)
		return &clean
	case StabilitySnapshot:
		return sanitizeSnapshot(typed)
	case []Recommendation:
		clean := make([]Recommendation, len(typed))
		for index, recommendation := range typed {
			clean[index] = sanitizeRecommendation(recommendation)
		}
		return clean
	case Recommendation:
		return sanitizeRecommendation(typed)
	case ScanProgress:
		return sanitizeProgress(typed)
	case NodeRecord:
		return sanitizeRecord(typed)
	default:
		return value
	}
}

func sanitizeReport(report Report) Report {
	clean := report
	clean.SourceEvidence = sanitizeEvidence(report.SourceEvidence)
	clean.RiskEvidence = sanitizeEvidence(report.RiskEvidence)
	clean.Errors = sanitizeErrors(report.Errors)
	if report.VendorResults != nil {
		clean.VendorResults = make(map[string]VendorResult, len(report.VendorResults))
		for name, result := range report.VendorResults {
			result.Errors = sanitizeErrors(result.Errors)
			clean.VendorResults[name] = result
		}
	}
	return clean
}

func sanitizeEvidence(evidence []SourceEvidence) []SourceEvidence {
	if evidence == nil {
		return nil
	}
	clean := make([]SourceEvidence, len(evidence))
	for index, item := range evidence {
		clean[index] = item
		clean[index].URL = redactURL(item.URL)
		if item.Error != nil {
			errorCopy := sanitizeReportError(*item.Error)
			clean[index].Error = &errorCopy
		}
	}
	return clean
}

func sanitizeErrors(items []ReportError) []ReportError {
	if items == nil {
		return nil
	}
	clean := make([]ReportError, len(items))
	for index, item := range items {
		clean[index] = sanitizeReportError(item)
	}
	return clean
}

func sanitizeReportError(item ReportError) ReportError {
	item.Source = redactURL(item.Source)
	item.Message = redactText(item.Message)
	return item
}

func sanitizeSnapshot(snapshot StabilitySnapshot) StabilitySnapshot { return snapshot }

func sanitizeRecommendation(recommendation Recommendation) Recommendation {
	recommendation.Reason = redactText(recommendation.Reason)
	return recommendation
}

func sanitizeProgress(progress ScanProgress) ScanProgress {
	if progress.Targets == nil {
		return progress
	}
	clean := progress
	clean.Targets = make(map[string]TargetScanProgress, len(progress.Targets))
	for key, value := range progress.Targets {
		clean.Targets[key] = value
	}
	return clean
}

func sanitizeRecord(record NodeRecord) NodeRecord {
	clean := record
	if record.Latest != nil {
		report := sanitizeReport(*record.Latest)
		clean.Latest = &report
	}
	if record.Best != nil {
		report := sanitizeReport(*record.Best)
		clean.Best = &report
	}
	if record.LastGood != nil {
		report := sanitizeReport(*record.LastGood)
		clean.LastGood = &report
	}
	return clean
}

func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.RawQuery == "" {
		return redactText(raw)
	}
	query := parsed.Query()
	for key := range query {
		query.Set(key, "<redacted>")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func redactText(raw string) string {
	return sensitiveAssignmentPattern.ReplaceAllString(raw, "$1=<redacted>")
}

func recommendationKey(recommendation Recommendation) string {
	if recommendation.ID != "" {
		return "id:" + recommendation.ID
	}
	return recommendation.Target + "\x00" + recommendation.Identity.ID()
}

func (s *Store) readRecommendations() ([]Recommendation, error) {
	return readRecommendationsForStore(s)
}
