package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/logging"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
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
	valueFlags := map[string]bool{"--config": true, "--data": true, "--logs": true, "--secret-file": true}
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
		return errors.New("usage: guardian run|status|switch|auto|probe|reload --config PATH")
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
