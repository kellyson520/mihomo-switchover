package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/logging"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
	"mihomo-guardian/internal/quality"
	"mihomo-guardian/internal/runtime"
	"mihomo-guardian/internal/state"
)

type runArgs struct {
	ConfigPath      string
	DataDir         string
	LogsDir         string
	SecretFile      string
	MihomoPath      string
	MihomoConfigDir string
}

type commandArgs struct {
	ConfigPath string
	DataDir    string
	LogsDir    string
	SecretFile string
}

type qualityDaemonArgs struct{ commandArgs }

type qualityRunArgs struct {
	commandArgs
	Target string
}

type qualityStatusArgs struct {
	ConfigPath string
	DataDir    string
}

type qualityBaselineResetArgs struct {
	ConfigPath string
	DataDir    string
	Target     string
	Node       string
	IP         string
}

func parseRunArgs(argv []string) (runArgs, error) {
	if len(argv) == 0 || argv[0] != "run" {
		return runArgs{}, errors.New("usage: guardian run --config PATH")
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	result := runArgs{}
	flags.StringVar(&result.ConfigPath, "config", "", "guardian config path")
	flags.StringVar(&result.DataDir, "data", "/guardian/data", "persistent data directory")
	flags.StringVar(&result.LogsDir, "logs", "/guardian/logs", "persistent log directory")
	flags.StringVar(&result.SecretFile, "secret-file", "/guardian/controller_secret", "mihomo API secret file")
	flags.StringVar(&result.MihomoPath, "mihomo", "/mihomo", "mihomo executable")
	flags.StringVar(&result.MihomoConfigDir, "mihomo-config", "/root/.config/mihomo", "mihomo config directory")
	if err := flags.Parse(argv[1:]); err != nil {
		return runArgs{}, err
	}
	if result.ConfigPath == "" {
		return runArgs{}, errors.New("--config is required")
	}
	return result, nil
}

func parseCommandArgs(command string, argv []string) (commandArgs, []string, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	result := commandArgs{}
	flags.StringVar(&result.ConfigPath, "config", "", "guardian config path")
	flags.StringVar(&result.DataDir, "data", "/guardian/data", "persistent data directory")
	flags.StringVar(&result.LogsDir, "logs", "/guardian/logs", "persistent log directory")
	flags.StringVar(&result.SecretFile, "secret-file", "/guardian/controller_secret", "mihomo API secret file")
	args := reorderCommandArgs(argv)
	if err := flags.Parse(args); err != nil {
		return commandArgs{}, nil, err
	}
	if result.ConfigPath == "" {
		return commandArgs{}, nil, errors.New("--config is required")
	}
	return result, flags.Args(), nil
}

func reorderCommandArgs(argv []string) []string {
	var flags, positional []string
	valueFlags := map[string]bool{"--config": true, "--data": true, "--logs": true, "--secret-file": true, "--target": true, "--node": true, "--ip": true}
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if valueFlags[arg] && index+1 < len(argv) {
			index++
			flags = append(flags, argv[index])
		}
	}
	return append(flags, positional...)
}

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(argv []string) error {
	if len(argv) == 0 {
		return errors.New("usage: guardian run|status|switch|auto|probe|reload|quality --config PATH")
	}
	if argv[0] == "quality-daemon" {
		args, err := parseQualityDaemonArgs(argv[1:])
		if err != nil {
			return err
		}
		return executeQualityDaemon(args)
	}
	if argv[0] == "quality" {
		return executeQualityCommand(argv[1:])
	}
	if argv[0] != "run" {
		args, rest, err := parseCommandArgs(argv[0], argv[1:])
		if err != nil {
			return err
		}
		switch argv[0] {
		case "status":
			return executeStatus(args)
		case "switch":
			if len(rest) != 1 {
				return errors.New("usage: guardian switch main|backup --config PATH")
			}
			return executeSwitch(args, rest[0])
		case "auto":
			if len(rest) != 0 {
				return errors.New("usage: guardian auto --config PATH")
			}
			return executeAuto(args)
		case "probe":
			if len(rest) != 0 {
				return errors.New("usage: guardian probe --config PATH")
			}
			return executeProbe(args)
		case "reload":
			if len(rest) != 0 {
				return errors.New("usage: guardian reload --config PATH")
			}
			return executeReload(args)
		default:
			return fmt.Errorf("unknown command %q", argv[0])
		}
	}
	args, err := parseRunArgs(argv)
	if err != nil {
		return err
	}
	return executeRun(args)
}

func parseQualityDaemonArgs(argv []string) (qualityDaemonArgs, error) {
	args, err := parseQualityExecutionArgs("quality-daemon", argv)
	if err != nil {
		return qualityDaemonArgs{}, err
	}
	return qualityDaemonArgs{commandArgs: args}, nil
}

func parseQualityRunArgs(argv []string) (qualityRunArgs, error) {
	flags := flag.NewFlagSet("quality run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	result := qualityRunArgs{commandArgs: commandArgs{}}
	flags.StringVar(&result.ConfigPath, "config", "", "guardian config path")
	flags.StringVar(&result.DataDir, "data", "", "persistent data directory")
	flags.StringVar(&result.LogsDir, "logs", "", "persistent log directory")
	flags.StringVar(&result.SecretFile, "secret-file", "", "mihomo API secret file")
	flags.StringVar(&result.Target, "target", "", "quality target id")
	if err := flags.Parse(reorderCommandArgs(argv)); err != nil {
		return qualityRunArgs{}, err
	}
	if len(flags.Args()) != 0 {
		return qualityRunArgs{}, errors.New("usage: guardian quality run --config PATH --data PATH --logs PATH --secret-file PATH [--target ID]")
	}
	if err := requireQualityExecutionPaths(result.commandArgs); err != nil {
		return qualityRunArgs{}, err
	}
	return result, nil
}

func parseQualityStatusArgs(argv []string) (qualityStatusArgs, error) {
	flags := flag.NewFlagSet("quality status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	result := qualityStatusArgs{}
	flags.StringVar(&result.ConfigPath, "config", "", "guardian config path")
	flags.StringVar(&result.DataDir, "data", "", "persistent data directory")
	if err := flags.Parse(reorderCommandArgs(argv)); err != nil {
		return qualityStatusArgs{}, err
	}
	if len(flags.Args()) != 0 {
		return qualityStatusArgs{}, errors.New("usage: guardian quality status --config PATH --data PATH")
	}
	if strings.TrimSpace(result.ConfigPath) == "" || strings.TrimSpace(result.DataDir) == "" {
		return qualityStatusArgs{}, errors.New("quality status requires --config and --data")
	}
	return result, nil
}

func parseQualityBaselineResetArgs(argv []string) (qualityBaselineResetArgs, error) {
	flags := flag.NewFlagSet("quality baseline-reset", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	result := qualityBaselineResetArgs{}
	flags.StringVar(&result.ConfigPath, "config", "", "guardian config path")
	flags.StringVar(&result.DataDir, "data", "", "persistent data directory")
	flags.StringVar(&result.Target, "target", "", "quality target id")
	flags.StringVar(&result.Node, "node", "", "exact node name")
	flags.StringVar(&result.IP, "ip", "", "exact exit IP")
	if err := flags.Parse(reorderCommandArgs(argv)); err != nil {
		return qualityBaselineResetArgs{}, err
	}
	if len(flags.Args()) != 0 {
		return qualityBaselineResetArgs{}, errors.New("usage: guardian quality baseline-reset --config PATH --data PATH --target ID --node NAME --ip ADDRESS")
	}
	if strings.TrimSpace(result.ConfigPath) == "" || strings.TrimSpace(result.DataDir) == "" ||
		strings.TrimSpace(result.Target) == "" || strings.TrimSpace(result.Node) == "" || strings.TrimSpace(result.IP) == "" {
		return qualityBaselineResetArgs{}, errors.New("quality baseline-reset requires --config, --data, --target, --node, and --ip")
	}
	if net.ParseIP(strings.TrimSpace(result.IP)) == nil {
		return qualityBaselineResetArgs{}, errors.New("quality baseline-reset --ip must be a valid IP address")
	}
	return result, nil
}

func parseQualityExecutionArgs(name string, argv []string) (commandArgs, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	result := commandArgs{}
	flags.StringVar(&result.ConfigPath, "config", "", "guardian config path")
	flags.StringVar(&result.DataDir, "data", "", "persistent data directory")
	flags.StringVar(&result.LogsDir, "logs", "", "persistent log directory")
	flags.StringVar(&result.SecretFile, "secret-file", "", "mihomo API secret file")
	if err := flags.Parse(reorderCommandArgs(argv)); err != nil {
		return commandArgs{}, err
	}
	if len(flags.Args()) != 0 {
		return commandArgs{}, fmt.Errorf("usage: guardian %s --config PATH --data PATH --logs PATH --secret-file PATH", name)
	}
	if err := requireQualityExecutionPaths(result); err != nil {
		return commandArgs{}, err
	}
	return result, nil
}

func requireQualityExecutionPaths(args commandArgs) error {
	missing := make([]string, 0, 4)
	if strings.TrimSpace(args.ConfigPath) == "" {
		missing = append(missing, "--config")
	}
	if strings.TrimSpace(args.DataDir) == "" {
		missing = append(missing, "--data")
	}
	if strings.TrimSpace(args.LogsDir) == "" {
		missing = append(missing, "--logs")
	}
	if strings.TrimSpace(args.SecretFile) == "" {
		missing = append(missing, "--secret-file")
	}
	if len(missing) != 0 {
		return fmt.Errorf("quality command requires %s", strings.Join(missing, ", "))
	}
	return nil
}

func executeQualityCommand(argv []string) error {
	if len(argv) == 0 {
		return errors.New("usage: guardian quality run|status|baseline-reset")
	}
	switch argv[0] {
	case "run":
		args, err := parseQualityRunArgs(argv[1:])
		if err != nil {
			return err
		}
		return executeQualityRun(args)
	case "status":
		args, err := parseQualityStatusArgs(argv[1:])
		if err != nil {
			return err
		}
		return executeQualityStatus(args)
	case "baseline-reset":
		args, err := parseQualityBaselineResetArgs(argv[1:])
		if err != nil {
			return err
		}
		return executeQualityBaselineReset(args)
	default:
		return fmt.Errorf("unknown quality command %q", argv[0])
	}
}

func executeRun(args runArgs) error {
	loader := config.NewLoader(args.ConfigPath)
	cfg, err := loader.Load()
	if err != nil {
		return fmt.Errorf("load guardian config: %w", err)
	}
	secretPath := args.SecretFile
	if cfg.Mihomo.SecretFile != "" {
		secretPath = cfg.Mihomo.SecretFile
	}
	secretData, err := os.ReadFile(secretPath)
	if err != nil {
		return fmt.Errorf("read mihomo secret: %w", err)
	}
	api, err := mihomo.NewClient(cfg.Mihomo.API, strings.TrimSpace(string(secretData)), 5*time.Second)
	if err != nil {
		return err
	}
	external, err := probe.NewExternalClient(cfg.Mihomo.Proxy, 5*time.Second)
	if err != nil {
		return err
	}
	logger, err := logging.NewFileLogger(filepath.Join(args.LogsDir, "guardian.jsonl"), logging.LoggerConfig{MaxBytes: cfg.Logging.MaxBytes, Retain: cfg.Logging.Retain})
	if err != nil {
		return fmt.Errorf("open guardian log: %w", err)
	}
	store := state.NewStore(filepath.Join(args.DataDir, "state.json"), cfg.Groups.Main)
	service := runtime.NewService(cfg, api, external, store, logger)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	link := &serviceLink{api: api, service: service, loader: loader, cfg: cfg, logger: logger}
	startupCtx, startupCancel := context.WithTimeout(ctx, cfg.Decision.StartupAPITimeout)
	defer startupCancel()
	if err := api.Heartbeat(startupCtx); err != nil {
		return fmt.Errorf("mihomo API unavailable at startup: %w", err)
	}
	return runtime.NewRuntime(link, runtime.RuntimeConfig{LinkLossGrace: cfg.Decision.LinkLossGrace, Tick: cfg.Reload.CheckInterval}).Run(ctx)
}

func executeQualityRun(args qualityRunArgs) error {
	cfg, api, reports, guardianState, logger, err := loadQualityDependencies(args.commandArgs)
	if err != nil {
		return err
	}
	if !cfg.Quality.Enabled {
		fmt.Println("quality scanning is disabled")
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := runQualityScan(ctx, cfg, api, reports, guardianState, logger, args.Target); err != nil {
		return err
	}
	fmt.Println("quality scan completed")
	return nil
}

func executeQualityDaemon(args qualityDaemonArgs) error {
	loader := config.NewLoader(args.ConfigPath)
	cfg, err := loader.Load()
	if err != nil {
		return fmt.Errorf("load guardian config: %w", err)
	}
	api, reports, guardianState, logger, err := openQualityDependencies(args.commandArgs, cfg)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	for {
		var scanErr error
		if cfg.Quality.Enabled {
			scanErr = runQualityScan(ctx, cfg, api, reports, guardianState, logger, "")
			if errors.Is(scanErr, quality.ErrQualityLink) {
				_ = logger.Event("quality_daemon_link_lost", map[string]any{"error": scanErr.Error()})
				return scanErr
			}
			if scanErr != nil {
				_ = logger.Event("quality_scan_failed", map[string]any{"error": scanErr.Error()})
			}
		}

		delay := cfg.Quality.FullScanInterval
		if scanErr != nil {
			delay = cfg.Quality.RetryInterval
		}
		if delay <= 0 {
			delay = 24 * time.Hour
		}
		if err := waitQualityDaemon(ctx, delay); err != nil {
			return nil
		}

		next, changed, reloadErr := loader.ReloadIfChanged(cfg)
		if reloadErr != nil {
			_ = logger.Event("quality_config_reload_failed", map[string]any{"error": reloadErr.Error()})
			continue
		}
		if changed {
			if cfg.Mihomo.API != next.Mihomo.API || cfg.Mihomo.Proxy != next.Mihomo.Proxy {
				err := fmt.Errorf("%w: mihomo endpoint changed; restart quality daemon", quality.ErrQualityLink)
				_ = logger.Event("quality_daemon_link_lost", map[string]any{"error": err.Error()})
				return err
			}
			cfg = next
			_ = logger.Event("quality_config_reloaded", map[string]any{"enabled": cfg.Quality.Enabled})
		}
	}
}

func waitQualityDaemon(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runQualityScan(ctx context.Context, cfg config.Config, api quality.MihomoAPI, reports *quality.Store, guardianState *state.Store, logger *logging.Logger, targetID string) error {
	if strings.TrimSpace(targetID) != "" {
		target, ok := qualityTargetByID(cfg, targetID)
		if !ok {
			return fmt.Errorf("quality target %q is not configured", targetID)
		}
		scanner := newQualityScanner(api, reports, guardianState, logger)
		scanErr := scanner.ScanTarget(ctx, cfg, target)
		if !errors.Is(scanErr, quality.ErrQualityLink) {
			if recommendationErr := refreshQualityRecommendations(reports, cfg, []string{target.ID}, time.Now().UTC()); recommendationErr != nil && scanErr == nil {
				return recommendationErr
			}
		}
		return scanErr
	}

	scanner := newQualityScanner(api, reports, guardianState, logger)
	scanErr := scanner.Scan(ctx, cfg)
	if errors.Is(scanErr, quality.ErrQualityLink) {
		return scanErr
	}
	if recommendationErr := refreshQualityRecommendations(reports, cfg, cfg.Quality.Order, time.Now().UTC()); recommendationErr != nil && scanErr == nil {
		return recommendationErr
	}
	if retentionErr := reports.ApplyRetention(cfg.Quality.Retention, time.Now().UTC()); retentionErr != nil && scanErr == nil {
		return fmt.Errorf("apply quality retention: %w", retentionErr)
	}
	return scanErr
}

func newQualityScanner(api quality.MihomoAPI, reports *quality.Store, guardianState *state.Store, logger *logging.Logger) *quality.Scanner {
	return &quality.Scanner{API: api, Reports: reports, State: guardianState, Logger: logger}
}

func qualityTargetByID(cfg config.Config, targetID string) (config.QualityTarget, bool) {
	for _, target := range cfg.Quality.Targets {
		if target.ID == targetID {
			return target, true
		}
	}
	return config.QualityTarget{}, false
}

func openQualityDependencies(args commandArgs, cfg config.Config) (*mihomo.Client, *quality.Store, *state.Store, *logging.Logger, error) {
	secretPath := args.SecretFile
	if cfg.Mihomo.SecretFile != "" {
		secretPath = cfg.Mihomo.SecretFile
	}
	secretData, err := os.ReadFile(secretPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read mihomo secret: %w", err)
	}
	api, err := mihomo.NewClient(cfg.Mihomo.API, strings.TrimSpace(string(secretData)), 5*time.Second)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	reports, err := quality.OpenStore(filepath.Join(args.DataDir, "ipquality"))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("open quality store: %w", err)
	}
	logger, err := logging.NewFileLogger(filepath.Join(args.LogsDir, "quality.jsonl"), logging.LoggerConfig{MaxBytes: cfg.Logging.MaxBytes, Retain: cfg.Logging.Retain})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("open quality log: %w", err)
	}
	return api, reports, state.NewStore(filepath.Join(args.DataDir, "state.json"), cfg.Groups.Main), logger, nil
}

func loadQualityDependencies(args commandArgs) (config.Config, *mihomo.Client, *quality.Store, *state.Store, *logging.Logger, error) {
	cfg, err := config.LoadFile(args.ConfigPath)
	if err != nil {
		return config.Config{}, nil, nil, nil, nil, fmt.Errorf("load guardian config: %w", err)
	}
	api, reports, guardianState, logger, err := openQualityDependencies(args, cfg)
	if err != nil {
		return config.Config{}, nil, nil, nil, nil, err
	}
	return cfg, api, reports, guardianState, logger, nil
}

type qualityStatusOutput struct {
	Enabled         bool                        `json:"enabled"`
	Order           []string                    `json:"order"`
	Targets         []qualityTargetStatusOutput `json:"targets"`
	NodeRecords     int                         `json:"node_records"`
	Recommendations int                         `json:"recommendations"`
	ScanProgress    quality.ScanProgress        `json:"scan_progress"`
	GeneratedAt     time.Time                   `json:"generated_at"`
}

type qualityTargetStatusOutput struct {
	ID           string    `json:"id"`
	SourceGroup  string    `json:"source_group"`
	Provider     string    `json:"provider,omitempty"`
	Scope        string    `json:"scope"`
	Records      int       `json:"records"`
	Baselines    int       `json:"baselines"`
	LatestAt     time.Time `json:"latest_at,omitempty"`
	ScanComplete bool      `json:"scan_complete"`
}

func executeQualityStatus(args qualityStatusArgs) error {
	cfg, err := config.LoadFile(args.ConfigPath)
	if err != nil {
		return fmt.Errorf("load guardian config: %w", err)
	}
	reports, err := quality.OpenStore(filepath.Join(args.DataDir, "ipquality"))
	if err != nil {
		return fmt.Errorf("open quality store: %w", err)
	}
	records, err := reports.ListNodeRecords()
	if err != nil {
		return fmt.Errorf("read quality node records: %w", err)
	}
	recommendations, err := reports.LoadRecommendations()
	if err != nil {
		return fmt.Errorf("read quality recommendations: %w", err)
	}
	progress, err := reports.LoadScanProgress()
	if err != nil {
		return fmt.Errorf("read quality scan progress: %w", err)
	}
	status := qualityStatusOutput{Enabled: cfg.Quality.Enabled, Order: append([]string(nil), cfg.Quality.Order...), NodeRecords: len(records), Recommendations: len(recommendations), ScanProgress: progress, GeneratedAt: time.Now().UTC()}
	for _, target := range cfg.Quality.Targets {
		item := qualityTargetStatusOutput{ID: target.ID, SourceGroup: target.SourceGroup, Provider: target.Provider, Scope: target.Scope}
		for _, record := range records {
			if record.Identity.Target != target.ID {
				continue
			}
			item.Records++
			if record.Baseline != nil {
				item.Baselines++
			}
			if record.Latest != nil && record.Latest.ObservedAt.After(item.LatestAt) {
				item.LatestAt = record.Latest.ObservedAt
			}
		}
		if targetProgress, ok := progress.Targets[target.ID]; ok {
			item.ScanComplete = targetProgress.Complete
		}
		status.Targets = append(status.Targets, item)
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func refreshQualityRecommendations(reports *quality.Store, cfg config.Config, targetIDs []string, now time.Time) error {
	if reports == nil {
		return errors.New("quality report store is required")
	}
	existing, err := reports.LoadRecommendations()
	if err != nil {
		return err
	}
	selected := make(map[string]struct{}, len(targetIDs))
	for _, id := range targetIDs {
		selected[id] = struct{}{}
	}
	recommendations := make([]quality.Recommendation, 0, len(existing))
	for _, item := range existing {
		if _, replace := selected[item.Target]; !replace {
			recommendations = append(recommendations, item)
		}
	}
	targets := make(map[string]config.QualityTarget, len(cfg.Quality.Targets))
	for _, target := range cfg.Quality.Targets {
		targets[target.ID] = target
	}
	records, err := reports.ListNodeRecords()
	if err != nil {
		return err
	}
	for _, record := range records {
		if _, ok := selected[record.Identity.Target]; !ok || record.Latest == nil {
			continue
		}
		target, ok := targets[record.Identity.Target]
		if !ok {
			continue
		}
		recommendation, err := quality.GenerateRecommendation(*record.Latest, record, target, now)
		if err != nil {
			continue
		}
		recommendations = append(recommendations, recommendation)
	}
	sort.SliceStable(recommendations, func(i, j int) bool {
		if recommendations[i].Target != recommendations[j].Target {
			return recommendations[i].Target < recommendations[j].Target
		}
		if recommendations[i].EffectiveScore != recommendations[j].EffectiveScore {
			return recommendations[i].EffectiveScore > recommendations[j].EffectiveScore
		}
		return recommendations[i].Node < recommendations[j].Node
	})
	return reports.SaveRecommendations(recommendations)
}

func executeQualityBaselineReset(args qualityBaselineResetArgs) error {
	cfg, err := config.LoadFile(args.ConfigPath)
	if err != nil {
		return fmt.Errorf("load guardian config: %w", err)
	}
	if _, ok := qualityTargetByID(cfg, args.Target); !ok {
		return fmt.Errorf("quality target %q is not configured", args.Target)
	}
	reports, err := quality.OpenStore(filepath.Join(args.DataDir, "ipquality"))
	if err != nil {
		return fmt.Errorf("open quality store: %w", err)
	}
	wantedIP := net.ParseIP(args.IP)
	if wantedIP == nil {
		return errors.New("baseline reset IP is invalid")
	}
	records, err := reports.ListNodeRecords()
	if err != nil {
		return fmt.Errorf("read quality node records: %w", err)
	}
	var matches []quality.NodeRecord
	for _, record := range records {
		identityIP := net.ParseIP(record.Identity.IP)
		if record.Identity.Target == args.Target && record.Identity.Node == args.Node && identityIP != nil && identityIP.String() == wantedIP.String() {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("quality identity not found for target=%q node=%q ip=%q", args.Target, args.Node, wantedIP.String())
	}
	if len(matches) != 1 {
		return fmt.Errorf("quality identity is ambiguous for target=%q node=%q ip=%q", args.Target, args.Node, wantedIP.String())
	}
	oldScore, newScore, err := resetQualityBaseline(reports, matches[0].Identity, time.Now().UTC())
	if err != nil {
		return err
	}
	result, _ := json.Marshal(map[string]any{"target": args.Target, "node": args.Node, "ip": wantedIP.String(), "old_baseline_score": oldScore, "new_baseline_score": newScore})
	fmt.Println(string(result))
	return nil
}

func resetQualityBaseline(reports *quality.Store, key quality.NodeKey, now time.Time) (int, int, error) {
	if reports == nil {
		return 0, 0, errors.New("quality report store is required")
	}
	var oldScore, newScore int
	err := withQualityStoreLock(reports, func() error {
		path := reports.NodePath(key)
		original, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record quality.NodeRecord
		if err := json.Unmarshal(original, &record); err != nil {
			return fmt.Errorf("decode quality node record: %w", err)
		}
		if record.Identity.ID() != key.ID() || record.Identity.Target != key.Target || record.Identity.Node != key.Node || record.Identity.IP != key.IP {
			return errors.New("quality node identity changed during baseline reset")
		}
		if record.Latest == nil {
			return errors.New("quality node has no latest report; refusing baseline reset")
		}
		if record.Latest.Identity.ID() != key.ID() {
			return errors.New("latest report identity does not match baseline reset identity")
		}
		if record.Baseline != nil {
			oldScore = record.Baseline.Score
		}
		newScore = record.Latest.EffectiveScore
		observedAt := record.Latest.ObservedAt
		if observedAt.IsZero() {
			observedAt = now
		}
		record.Baseline = &quality.Baseline{Identity: key, Score: newScore, QualityScore: record.Latest.QualityScore, StabilityScore: record.Latest.StabilityScore, ConfidencePercent: record.Latest.ConfidencePercent, CreatedAt: now, ObservedAt: observedAt}
		if err := writeQualityJSONAtomic(path, record); err != nil {
			return fmt.Errorf("write reset baseline: %w", err)
		}
		if err := appendQualityAudit(reports, qualityBaselineAudit{Event: "baseline_reset", At: now, Identity: key, OldScore: oldScore, NewScore: newScore}); err != nil {
			_ = writeQualityBytesAtomic(path, original)
			return fmt.Errorf("write baseline reset audit: %w", err)
		}
		return nil
	})
	return oldScore, newScore, err
}

type qualityBaselineAudit struct {
	Event    string          `json:"event"`
	At       time.Time       `json:"at"`
	Identity quality.NodeKey `json:"identity"`
	OldScore int             `json:"old_baseline_score"`
	NewScore int             `json:"new_baseline_score"`
}

func withQualityStoreLock(reports *quality.Store, fn func() error) error {
	lock, err := os.OpenFile(reports.ScanLockPath(), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lock.Chmod(0600); err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func writeQualityJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeQualityBytesAtomic(path, data)
}

func writeQualityBytesAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".guardian-quality.tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
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
	return directoryFile.Sync()
}

func appendQualityAudit(reports *quality.Store, event qualityBaselineAudit) error {
	path := filepath.Join(reports.Root(), "audit.jsonl")
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func loadCommandDependencies(args commandArgs, external, openLogger bool) (config.Config, *mihomo.Client, *probe.ExternalClient, *state.Store, *logging.Logger, error) {
	cfg, err := config.LoadFile(args.ConfigPath)
	if err != nil {
		return config.Config{}, nil, nil, nil, nil, fmt.Errorf("load guardian config: %w", err)
	}
	secretPath := args.SecretFile
	if cfg.Mihomo.SecretFile != "" {
		secretPath = cfg.Mihomo.SecretFile
	}
	secretData, err := os.ReadFile(secretPath)
	if err != nil {
		return config.Config{}, nil, nil, nil, nil, fmt.Errorf("read mihomo secret: %w", err)
	}
	api, err := mihomo.NewClient(cfg.Mihomo.API, strings.TrimSpace(string(secretData)), 5*time.Second)
	if err != nil {
		return config.Config{}, nil, nil, nil, nil, err
	}
	var externalClient *probe.ExternalClient
	if external {
		externalClient, err = probe.NewExternalClient(cfg.Mihomo.Proxy, 5*time.Second)
		if err != nil {
			return config.Config{}, nil, nil, nil, nil, err
		}
	}
	var logger *logging.Logger
	if openLogger {
		logger, err = logging.NewFileLogger(filepath.Join(args.LogsDir, "guardian.jsonl"), logging.LoggerConfig{MaxBytes: cfg.Logging.MaxBytes, Retain: cfg.Logging.Retain})
		if err != nil {
			return config.Config{}, nil, nil, nil, nil, fmt.Errorf("open guardian log: %w", err)
		}
	}
	return cfg, api, externalClient, state.NewStore(filepath.Join(args.DataDir, "state.json"), cfg.Groups.Main), logger, nil
}

func executeStatus(args commandArgs) error {
	cfg, api, _, store, _, err := loadCommandDependencies(args, false, false)
	if err != nil {
		return err
	}
	if err := api.Heartbeat(context.Background()); err != nil {
		return fmt.Errorf("mihomo API unavailable: %w", err)
	}
	channel, err := api.GetProxy(context.Background(), cfg.Groups.Channel)
	if err != nil {
		return err
	}
	mainGroup, err := api.GetProxy(context.Background(), cfg.Groups.Main)
	if err != nil {
		return err
	}
	backupGroup, err := api.GetProxy(context.Background(), cfg.Groups.Backup)
	if err != nil {
		return err
	}
	stored, err := store.Load()
	if err != nil {
		return err
	}
	fmt.Printf("channel=%s\nmain=%s\nbackup=%s\nstate_channel=%s\n", channel.Now, mainGroup.Now, backupGroup.Now, stored.CurrentChannel)
	return nil
}

func executeSwitch(args commandArgs, target string) error {
	cfg, api, _, store, logger, err := loadCommandDependencies(args, false, true)
	if err != nil {
		return err
	}
	target = strings.ToLower(strings.TrimSpace(target))
	channel := cfg.Groups.Main
	if target == "backup" {
		channel = cfg.Groups.Backup
	} else if target != "main" {
		return errors.New("switch target must be main or backup")
	}
	if err := api.Heartbeat(context.Background()); err != nil {
		return err
	}
	group, err := api.GetProxy(context.Background(), cfg.Groups.Channel)
	if err != nil {
		return err
	}
	if !contains(group.All, channel) {
		return fmt.Errorf("channel group does not contain %q; refusing manual switch", channel)
	}
	if err := api.SetProxy(context.Background(), cfg.Groups.Channel, channel); err != nil {
		return err
	}
	stored, err := store.Load()
	if err != nil {
		return err
	}
	stored.CurrentChannel = channel
	stored.FailureStreak = 0
	stored.RecoveryStreak = 0
	stored.ForcedChannel = channel
	stored.ForceUntil = time.Now().UTC().Add(30 * time.Minute)
	if err := store.Save(stored); err != nil {
		return err
	}
	_ = logger.Event("manual_switch", map[string]any{"channel": channel, "force_until": stored.ForceUntil})
	fmt.Println("switched", channel)
	return nil
}

func executeAuto(args commandArgs) error {
	_, _, _, store, logger, err := loadCommandDependencies(args, false, true)
	if err != nil {
		return err
	}
	stored, err := store.Load()
	if err != nil {
		return err
	}
	stored.ForcedChannel = ""
	stored.ForceUntil = time.Time{}
	if err := store.Save(stored); err != nil {
		return err
	}
	_ = logger.Event("manual_auto", map[string]any{"channel": stored.CurrentChannel})
	fmt.Println("automatic mode enabled")
	return nil
}

func executeProbe(args commandArgs) error {
	cfg, api, external, _, logger, err := loadCommandDependencies(args, true, true)
	if err != nil {
		return err
	}
	if err := api.Heartbeat(context.Background()); err != nil {
		return err
	}
	for _, spec := range cfg.Probes {
		if !spec.Enabled {
			continue
		}
		result := external.Check(context.Background(), spec)
		_ = logger.Event("manual_probe", map[string]any{"probe": spec.ID, "class": result.Class, "status": result.Status, "duration_ms": result.Duration.Milliseconds(), "error": result.Err})
		data, _ := json.Marshal(map[string]any{"probe": spec.ID, "class": result.Class, "status": result.Status, "duration_ms": result.Duration.Milliseconds(), "error": result.Err})
		fmt.Println(string(data))
	}
	return nil
}

func executeReload(args commandArgs) error {
	if _, err := config.LoadFile(args.ConfigPath); err != nil {
		return fmt.Errorf("config is invalid: %w", err)
	}
	fmt.Println("configuration is valid; running guardian will apply it on its next reload check")
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

type serviceLink struct {
	api     *mihomo.Client
	service *runtime.Service
	loader  *config.Loader
	cfg     config.Config
	logger  *logging.Logger
}

func (l *serviceLink) Heartbeat(ctx context.Context) error { return l.api.Heartbeat(ctx) }

func (l *serviceLink) RunCycle(ctx context.Context) error { return l.service.RunCycle(ctx) }

func (l *serviceLink) CycleInterval() time.Duration { return l.cfg.Decision.Interval }

func (l *serviceLink) ReloadInterval() time.Duration { return l.cfg.Reload.CheckInterval }

func (l *serviceLink) Reload() error {
	next, changed, err := l.loader.ReloadIfChanged(l.cfg)
	if err != nil {
		if l.logger != nil {
			_ = l.logger.Event("config_reload_failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	if changed {
		if runtimeInfrastructureChanged(l.cfg, next) {
			return errors.New("mihomo endpoints, groups, providers, or logging require a guardian restart")
		}
		l.cfg = next
		l.service.UpdateConfig(next)
		if l.logger != nil {
			_ = l.logger.Event("config_reloaded", map[string]any{"mode": next.Decision.Mode})
		}
	}
	return nil
}

func runtimeInfrastructureChanged(before, after config.Config) bool {
	return before.Mihomo != after.Mihomo ||
		before.Groups != after.Groups ||
		before.Providers != after.Providers ||
		before.Logging != after.Logging
}
