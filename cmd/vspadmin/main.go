// Copyright (c) 2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/hdkeychain/v3"
	"github.com/decred/vspd/database"
	"github.com/decred/vspd/internal/config"
	"github.com/decred/vspd/internal/vspd"
	"github.com/jessevdk/go-flags"
)

const (
	configFilename = "vspd.conf"
	dbFilename     = "vspd.db"
)

type conf struct {
	HomeDir string `long:"homedir" description:"Path to application home directory."`
	Network string `long:"network" description:"Decred network to use." choice:"mainnet" choice:"testnet" choice:"simnet"`
}

var defaultConf = conf{
	HomeDir: dcrutil.AppDataDir("vspd", false),
	Network: "mainnet",
}

func log(format string, a ...any) {
	fmt.Printf(format, a...)
	fmt.Println()
}

// fileExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); os.IsNotExist(err) {
		return false
	}
	return true
}

func createDatabase(homeDir string, feeXPub string, network *config.Network) error {
	dataDir := filepath.Join(homeDir, "data", network.Name)
	dbFile := filepath.Join(dataDir, dbFilename)

	// Return error if database already exists.
	if fileExists(dbFile) {
		return fmt.Errorf("database already exists in %s", dataDir)
	}

	// Ensure provided xpub is a valid key for the selected network.
	_, err := hdkeychain.NewKeyFromString(feeXPub, network.Params)
	if err != nil {
		return fmt.Errorf("failed to parse feexpub: %w", err)
	}

	// Ensure the data directory exists.
	err = os.MkdirAll(dataDir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Create new database.
	err = database.CreateNew(dbFile, feeXPub)
	if err != nil {
		return fmt.Errorf("error creating db file %s: %w", dbFile, err)
	}

	log("New %s vspd database created in %s", network.Name, homeDir)

	return nil
}

func writeConfig(homeDir string) error {
	configFile := filepath.Join(homeDir, configFilename)

	// Return an error if the config file already exists.
	if fileExists(configFile) {
		return fmt.Errorf("config file already exists in %s", homeDir)
	}

	// Ensure the directory exists.
	err := os.MkdirAll(homeDir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Write a config file with default values to the provided home directory.
	preParser := flags.NewParser(&vspd.DefaultConfig, flags.None)
	preIni := flags.NewIniParser(preParser)
	err = preIni.WriteFile(configFile,
		flags.IniIncludeComments|flags.IniIncludeDefaults)
	if err != nil {
		return fmt.Errorf("error creating a default "+
			"config file: %w", err)
	}

	log("Config file with default values written to %s", configFile)
	log("Fill in the blanks")

	return nil
}

// run is the real main function for vspadmin. It is necessary to work around
// the fact that deferred functions do not run when os.Exit() is called.
func run() int {
	cfg := defaultConf

	// If command line options are requesting help, write it to stdout and exit.
	if config.WriteHelp(&cfg) {
		return 0
	}

	// Parse command line options.
	remainingArgs, err := flags.Parse(&cfg)
	if err != nil {
		log("%v", err)
		return 1
	}

	network, err := config.NetworkFromName(cfg.Network)
	if err != nil {
		log("%v", err)
		return 1
	}

	if len(remainingArgs) < 1 {
		log("No command specified")
		return 1
	}

	switch remainingArgs[0] {
	case "createdatabase":
		if len(remainingArgs) != 2 {
			log("createdatabase has one required argument, fee xpub")
			return 1
		}

		feeXPub := remainingArgs[1]

		err = createDatabase(cfg.HomeDir, feeXPub, network)
		if err != nil {
			log("createdatabase: %v", err)
			return 1
		}

	case "writeconfig":
		err = writeConfig(cfg.HomeDir)
		if err != nil {
			log("writeconfig: %v", err)
			return 1
		}

	default:
		log("%q is not a valid command", remainingArgs[0])
		return 1
	}

	return 0
}

func main() {
	os.Exit(run())
}
