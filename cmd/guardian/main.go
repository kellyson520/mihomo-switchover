package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/logging"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
	"mihomo-guardian/internal/runtime"
	"mihomo-guardian/internal/state"
	"mihomo-guardian/internal/supervisor"
)

type runArgs struct {
	ConfigPath      string
	DataDir         string
	LogsDir         string
	SecretFile      string
	MihomoPath      string
	MihomoConfigDir string
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

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(argv []string) error {
	if len(argv) == 0 {
		return errors.New("usage: guardian run --config PATH")
	}
	args, err := parseRunArgs(argv)
	if err != nil {
		return err
	}
	cfg, err := config.LoadFile(args.ConfigPath)
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
	child := supervisor.NewExecChild(args.MihomoPath, "-d", args.MihomoConfigDir)
	sup := supervisor.New(child, supervisor.SupervisorConfig{StartupAPITimeout: cfg.Decision.LinkLossGrace})
	ctx, cancel := supervisor.SignalsContext(context.Background())
	defer cancel()
	link := &serviceLink{api: api, service: service}
	return sup.Run(ctx, func(readyCtx context.Context) error {
		if err := api.Heartbeat(readyCtx); err != nil {
			return fmt.Errorf("mihomo API unavailable at startup: %w", err)
		}
		return nil
	}, func(workCtx context.Context, terminate func() error) error {
		link.terminate = terminate
		return runtime.NewRuntime(link, runtime.RuntimeConfig{LinkLossGrace: cfg.Decision.LinkLossGrace, Tick: cfg.Decision.Interval}).Run(workCtx)
	})
}

type serviceLink struct {
	api       *mihomo.Client
	service   *runtime.Service
	terminate func() error
}

func (l *serviceLink) Heartbeat(ctx context.Context) error { return l.api.Heartbeat(ctx) }

func (l *serviceLink) RunCycle(ctx context.Context) error { return l.service.RunCycle(ctx) }

func (l *serviceLink) TerminateMihomo(context.Context) error {
	if l.terminate == nil {
		return errors.New("mihomo terminator is not ready")
	}
	return l.terminate()
}
