// Copyright (c) 2020-2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/decred/dcrd/wire"
	"github.com/decred/slog"
	"github.com/decred/vspd/database"
	"github.com/decred/vspd/internal/config"
	"github.com/decred/vspd/internal/signal"
	"github.com/decred/vspd/internal/version"
	"github.com/decred/vspd/internal/vspd"
	"github.com/decred/vspd/internal/webapi"
	"github.com/decred/vspd/rpc"
)

const (
	// maxVoteChangeRecords defines how many vote change records will be stored
	// for each ticket. The limit is in place to mitigate DoS attacks on server
	// storage space. When storing a new record breaches this limit, the oldest
	// record in the database is deleted.
	maxVoteChangeRecords = 10
)

func main() {
	os.Exit(run())
}

// initLogging uses the provided vspd config to create a logging backend, and
// returns a function which can be used to create ready-to-use subsystem
// loggers.
func initLogging(cfg *vspdConfig) (func(subsystem string) slog.Logger, error) {
	backend, err := newLogBackend(cfg.logPath, "vspd", cfg.MaxLogSize, cfg.LogsToKeep)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize logger: %w", err)
	}

	var ok bool
	level, ok := slog.LevelFromString(cfg.LogLevel)
	if !ok {
		return nil, fmt.Errorf("unknown log level: %q", cfg.LogLevel)
	}

	return func(subsystem string) slog.Logger {
		log := backend.Logger(subsystem)
		log.SetLevel(level)
		return log
	}, nil
}

// run is the real main function for vspd. It is necessary to work around the
// fact that deferred functions do not run when os.Exit() is called.
func run() int {
	// Load config file and parse CLI args.
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadConfig error: %v\n", err)
		return 1
	}

	makeLogger, err := initLogging(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initLogging error: %v\n", err)
		return 1
	}

	log := makeLogger("VSP")

	// Create a context that is canceled when a shutdown request is received
	// through an interrupt signal such as SIGINT (Ctrl+C).
	ctx := signal.ShutdownListener(log)

	defer log.Criticalf("Shutdown complete")
	log.Criticalf("Version %s (Go version %s %s/%s)", version.String(),
		runtime.Version(), runtime.GOOS, runtime.GOARCH)

	if cfg.network == &config.MainNet && version.IsPreRelease() {
		log.Warnf("")
		log.Warnf("\tWARNING: This is a pre-release version of vspd which should not be used on mainnet")
		log.Warnf("")
	}

	if cfg.VspClosed {
		log.Warnf("")
		log.Warnf("\tWARNING: Config --vspclosed is set. This will prevent vspd from accepting new tickets")
		log.Warnf("")
	}

	if cfg.ConfigFile != "" {
		log.Warnf("")
		log.Warnf("\tWARNING: Config --configfile is set. This is a deprecated option which has no effect and will be removed in a future release")
		log.Warnf("")
	}

	if cfg.FeeXPub != "" {
		log.Warnf("")
		log.Warnf("\tWARNING: Config --feexpub is set. This behavior has been moved into vspadmin and will be removed from vspd in a future release")
		log.Warnf("")
	}

	// Open database.
	db, err := database.Open(cfg.dbPath, makeLogger(" DB"), maxVoteChangeRecords)
	if err != nil {
		log.Errorf("Failed to open database: %v", err)
		return 1
	}
	const writeBackup = true
	defer db.Close(writeBackup)

	rpcLog := makeLogger("RPC")

	// Create a channel to receive blockConnected notifications from dcrd.
	blockNotifChan := make(chan *wire.BlockHeader)

	// Create RPC client for local dcrd instance (used for broadcasting and
	// checking the status of fee transactions).
	dcrd := rpc.SetupDcrd(cfg.DcrdUser, cfg.DcrdPass, cfg.DcrdHost, cfg.dcrdCert, cfg.network.Params, rpcLog, blockNotifChan)
	defer dcrd.Close()

	// Create RPC client for remote dcrwallet instances (used for voting).
	wallets := rpc.SetupWallet(cfg.walletUsers, cfg.walletPasswords, cfg.walletHosts, cfg.walletCerts, cfg.network.Params, rpcLog)
	defer wallets.Close()

	// Create webapi server.
	apiCfg := webapi.Config{
		Listen:               cfg.Listen,
		VSPFee:               cfg.VSPFee,
		Network:              cfg.network,
		SupportEmail:         cfg.SupportEmail,
		VspClosed:            cfg.VspClosed,
		VspClosedMsg:         cfg.VspClosedMsg,
		AdminPass:            cfg.AdminPass,
		Debug:                cfg.WebServerDebug,
		Designation:          cfg.Designation,
		MaxVoteChangeRecords: maxVoteChangeRecords,
		VspdVersion:          version.String(),
	}
	api, err := webapi.New(db, makeLogger("API"), dcrd, wallets, apiCfg)
	if err != nil {
		log.Errorf("Failed to initialize webapi: %v", err)
		return 1
	}

	// WaitGroup for services to signal when they have shutdown cleanly.
	var wg sync.WaitGroup

	// Start the webapi server.
	wg.Add(1)
	go func() {
		api.Run(ctx)
		wg.Done()
	}()

	// Start vspd.
	vspd := vspd.New(cfg.network, log, db, dcrd, wallets, blockNotifChan)
	wg.Add(1)
	go func() {
		vspd.Run(ctx)
		wg.Done()
	}()

	// Periodically write a database backup file.
	wg.Add(1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				wg.Done()
				return
			case <-time.After(cfg.BackupInterval):
				err := db.WriteHotBackupFile()
				if err != nil {
					log.Errorf("Failed to write database backup: %v", err)
				}
			}
		}
	}()

	// Wait for shutdown tasks to complete before running deferred tasks and
	// returning.
	wg.Wait()

	return 0
}
