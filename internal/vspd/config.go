// Copyright (c) 2021-2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package vspd

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/vspd/internal/config"
	"github.com/decred/vspd/internal/version"
	flags "github.com/jessevdk/go-flags"
)

const (
	configFilename = "vspd.conf"
	dbFilename     = "vspd.db"
)

// Config defines the configuration options for the vspd process.
type Config struct {
	Listen          string        `long:"listen" ini-name:"listen" description:"The ip:port to listen for API requests."`
	LogLevel        string        `long:"loglevel" ini-name:"loglevel" description:"Logging level." choice:"trace" choice:"debug" choice:"info" choice:"warn" choice:"error" choice:"critical"`
	MaxLogSize      int64         `long:"maxlogsize" ini-name:"maxlogsize" description:"File size threshold for log file rotation (MB)."`
	LogsToKeep      int           `long:"logstokeep" ini-name:"logstokeep" description:"The number of rotated log files to keep."`
	NetworkName     string        `long:"network" ini-name:"network" description:"Decred network to use." choice:"testnet" choice:"mainnet" choice:"simnet"`
	VSPFee          float64       `long:"vspfee" ini-name:"vspfee" description:"Fee percentage charged for VSP use. eg. 2.0 (2%), 0.5 (0.5%)."`
	DcrdHost        string        `long:"dcrdhost" ini-name:"dcrdhost" description:"The ip:port to establish a JSON-RPC connection with dcrd. Should be the same host where vspd is running."`
	DcrdUser        string        `long:"dcrduser" ini-name:"dcrduser" description:"Username for dcrd RPC connections."`
	DcrdPass        string        `long:"dcrdpass" ini-name:"dcrdpass" description:"Password for dcrd RPC connections."`
	DcrdCert        string        `long:"dcrdcert" ini-name:"dcrdcert" description:"The dcrd RPC certificate file."`
	WalletHosts     string        `long:"wallethost" ini-name:"wallethost" description:"Comma separated list of ip:port to establish JSON-RPC connections with voting dcrwallet."`
	WalletUsers     string        `long:"walletuser" ini-name:"walletuser" description:"Comma separated list of username for dcrwallet RPC connections."`
	WalletPasswords string        `long:"walletpass" ini-name:"walletpass" description:"Comma separated list of password for dcrwallet RPC connections."`
	WalletCerts     string        `long:"walletcert" ini-name:"walletcert" description:"Comma separated list of dcrwallet RPC certificate files."`
	WebServerDebug  bool          `long:"webserverdebug" ini-name:"webserverdebug" description:"Enable web server debug mode (verbose logging to terminal and live-reloading templates)."`
	SupportEmail    string        `long:"supportemail" ini-name:"supportemail" description:"Email address for users in need of support."`
	BackupInterval  time.Duration `long:"backupinterval" ini-name:"backupinterval" description:"Time period between automatic database backups. Valid time units are {s,m,h}. Minimum 30 seconds."`
	VspClosed       bool          `long:"vspclosed" ini-name:"vspclosed" description:"Closed prevents the VSP from accepting new tickets."`
	VspClosedMsg    string        `long:"vspclosedmsg" ini-name:"vspclosedmsg" description:"A short message displayed on the webpage and returned by the status API endpoint if vspclosed is true."`
	AdminPass       string        `long:"adminpass" ini-name:"adminpass" description:"Password for accessing admin page."`
	Designation     string        `long:"designation" ini-name:"designation" description:"Short name for the VSP. Customizes the logo in the top toolbar."`

	// The following flags should be set on CLI only, not via config file.
	ShowVersion bool   `long:"version" no-ini:"true" description:"Display version information and exit."`
	FeeXPub     string `long:"feexpub" no-ini:"true" description:"DEPRECATED: This behavior has been moved into vspadmin and will be removed from vspd in a future version of the software."`
	HomeDir     string `long:"homedir" no-ini:"true" description:"Path to application home directory. Used for storing VSP database and logs."`
	ConfigFile  string `long:"configfile" no-ini:"true" description:"DEPRECATED: This behavior is no longer available and this option will be removed in a future version of the software."`

	LogPath                                      string
	DbPath                                       string
	Network                                      *config.Network
	DcrdCert2                                    []byte
	WalletHosts2, WalletUsers2, WalletPasswords2 []string
	WalletCerts2                                 [][]byte
}

var DefaultConfig = Config{
	Listen:         ":8800",
	LogLevel:       "debug",
	MaxLogSize:     int64(10),
	LogsToKeep:     20,
	NetworkName:    "testnet",
	VSPFee:         3.0,
	HomeDir:        dcrutil.AppDataDir("vspd", false),
	DcrdHost:       "127.0.0.1",
	WalletHosts:    "127.0.0.1",
	WebServerDebug: false,
	BackupInterval: time.Minute * 3,
	VspClosed:      false,
	Designation:    "Voting Service Provider",
}

// fileExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); os.IsNotExist(err) {
		return false
	}
	return true
}

// cleanAndExpandPath expands environment variables and leading ~ in the
// passed path, cleans the result, and returns it.
func cleanAndExpandPath(path string) string {
	// Nothing to do when no path is given.
	if path == "" {
		return path
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows cmd.exe-style
	// %VARIABLE%, but the variables can still be expanded via POSIX-style
	// $VARIABLE.
	path = os.ExpandEnv(path)

	if !strings.HasPrefix(path, "~") {
		return filepath.Clean(path)
	}

	// Expand initial ~ to the current user's home directory, or ~otheruser
	// to otheruser's home directory.  On Windows, both forward and backward
	// slashes can be used.
	path = path[1:]

	var pathSeparators string
	if runtime.GOOS == "windows" {
		pathSeparators = string(os.PathSeparator) + "/"
	} else {
		pathSeparators = string(os.PathSeparator)
	}

	userName := ""
	if i := strings.IndexAny(path, pathSeparators); i != -1 {
		userName = path[:i]
		path = path[i:]
	}

	homeDir := ""
	var u *user.User
	var err error
	if userName == "" {
		u, err = user.Current()
	} else {
		u, err = user.Lookup(userName)
	}
	if err == nil {
		homeDir = u.HomeDir
	}
	// Fallback to CWD if user lookup fails or user has no home directory.
	if homeDir == "" {
		homeDir = "."
	}

	return filepath.Join(homeDir, path)
}

// normalizeAddress returns addr with the passed default port appended if
// there is not already a port specified.
func normalizeAddress(addr, defaultPort string) string {
	_, _, err := net.SplitHostPort(addr)
	if err != nil {
		return net.JoinHostPort(addr, defaultPort)
	}
	return addr
}

// LoadConfig initializes and parses the config using a config file and command
// line options.
//
// The configuration proceeds as follows:
//  1. Start with a default config with sane settings
//  2. Pre-parse the command line to check for an alternative config file
//  3. Load configuration file overwriting defaults with any specified options
//  4. Parse CLI options and overwrite/add any specified options
//
// The above results in vspd functioning properly without any config settings
// while still allowing the user to override settings with config files and
// command line options.  Command line options always take precedence.
func LoadConfig() (*Config, error) {
	cfg := DefaultConfig

	// If command line options are requesting help, write it to stdout and exit.
	if config.WriteHelp(&cfg) {
		os.Exit(0)
	}

	// Pre-parse the command line options to see if an alternative home dir or
	// the version flag were specified.
	preCfg := cfg

	preParser := flags.NewParser(&preCfg, flags.None)
	_, err := preParser.Parse()
	if err != nil {
		return nil, err
	}

	appName := filepath.Base(os.Args[0])

	// Show the version and exit if the version flag was specified.
	if preCfg.ShowVersion {
		fmt.Printf("%s version %s (Go version %s %s/%s)\n", appName,
			version.String(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	appName = strings.TrimSuffix(appName, filepath.Ext(appName))
	usageMessage := fmt.Sprintf("Use %s -h to show usage", appName)

	// Update the home directory if specified on CLI.
	if preCfg.HomeDir != "" {
		cfg.HomeDir = cleanAndExpandPath(preCfg.HomeDir)
	}

	// Create the home directory if it doesn't already exist.
	err = os.MkdirAll(cfg.HomeDir, 0700)
	if err != nil {
		err := fmt.Errorf("failed to create home directory: %w", err)
		fmt.Fprintln(os.Stderr, err)
		return nil, err
	}

	// Create a default config file when one does not exist and the user did
	// not specify an override.
	configFile := filepath.Join(cfg.HomeDir, configFilename)
	if !fileExists(configFile) {
		preIni := flags.NewIniParser(preParser)
		err = preIni.WriteFile(configFile,
			flags.IniIncludeComments|flags.IniIncludeDefaults)
		if err != nil {
			return nil, fmt.Errorf("error creating a default "+
				"config file: %w", err)
		}
		fmt.Printf("Config file with default values written to %s\n", configFile)

		// File created, user now has to fill in values. Proceeding with the
		// default file just causes errors.
		os.Exit(0)
	}

	// Load additional config from file.
	parser := flags.NewParser(&cfg, flags.None)

	err = flags.NewIniParser(parser).ParseFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("error parsing config file: %w", err)
	}

	// Parse command line options again to ensure they take precedence.
	_, err = parser.Parse()
	if err != nil {
		fmt.Fprintln(os.Stderr, usageMessage)
		return nil, err
	}

	// Set the active network.
	cfg.Network, err = config.NetworkFromName(cfg.NetworkName)
	if err != nil {
		return nil, err
	}

	// Ensure backup interval is greater than 30 seconds.
	if cfg.BackupInterval < time.Second*30 {
		return nil, errors.New("minimum backupinterval is 30 seconds")
	}

	// validPoolFeeRate tests to see if a pool fee is a valid percentage from
	// 0.01% to 100.00%.
	validPoolFeeRate := func(feeRate float64) bool {
		poolFeeRateTest := feeRate * 100
		poolFeeRateTest = math.Floor(poolFeeRateTest)
		return poolFeeRateTest >= 1.0 && poolFeeRateTest <= 10000.0
	}

	// Ensure the fee percentage is valid per txrules.
	if !validPoolFeeRate(cfg.VSPFee) {
		return nil, errors.New("invalid vspfee - should be greater than 0.01 and less than 100.0")
	}

	// If VSP is not closed, ignore any provided closure message.
	if !cfg.VspClosed {
		cfg.VspClosedMsg = ""
	}

	// Ensure the support email address is set.
	if cfg.SupportEmail == "" {
		return nil, errors.New("the supportemail option is not set")
	}

	// Ensure the administrator password is set.
	if cfg.AdminPass == "" {
		return nil, errors.New("the adminpass option is not set")
	}

	// Ensure the dcrd RPC username is set.
	if cfg.DcrdUser == "" {
		return nil, errors.New("the dcrduser option is not set")
	}

	// Ensure the dcrd RPC password is set.
	if cfg.DcrdPass == "" {
		return nil, errors.New("the dcrdpass option is not set")
	}

	// Ensure the dcrd RPC cert path is set.
	if cfg.DcrdCert == "" {
		return nil, errors.New("the dcrdcert option is not set")
	}

	// Load dcrd RPC certificate.
	cfg.DcrdCert = cleanAndExpandPath(cfg.DcrdCert)
	cfg.DcrdCert2, err = os.ReadFile(cfg.DcrdCert)
	if err != nil {
		return nil, fmt.Errorf("failed to read dcrd cert file: %w", err)
	}

	// Ensure the dcrwallet RPC username is set.
	if cfg.WalletUsers == "" {
		return nil, errors.New("the walletuser option is not set")
	}

	// Ensure the dcrwallet RPC password is set.
	if cfg.WalletPasswords == "" {
		return nil, errors.New("the walletpass option is not set")
	}

	// Ensure the dcrwallet RPC cert path is set.
	if cfg.WalletCerts == "" {
		return nil, errors.New("the walletcert option is not set")
	}

	// Parse list of wallet hosts.
	cfg.WalletHosts2 = strings.Split(cfg.WalletHosts, ",")
	numHost := len(cfg.WalletHosts2)

	// An RPC username must be specified for each wallet host.
	cfg.WalletUsers2 = strings.Split(cfg.WalletUsers, ",")
	numUser := len(cfg.WalletUsers2)
	if numUser != numHost {
		return nil, fmt.Errorf("%d wallet hosts specified, expected %d RPC usernames, got %d",
			numHost, numHost, numUser)
	}

	// An RPC password must be specified for each wallet host.
	cfg.WalletPasswords2 = strings.Split(cfg.WalletPasswords, ",")
	numPass := len(cfg.WalletPasswords2)
	if numPass != numHost {
		return nil, fmt.Errorf("%d wallet hosts specified, expected %d RPC passwords, got %d",
			numHost, numHost, numPass)
	}

	// An RPC certificate must be specified for each wallet host.
	certs := strings.Split(cfg.WalletCerts, ",")
	numCert := len(certs)
	if numCert != numHost {
		return nil, fmt.Errorf("%d wallet hosts specified, expected %d RPC certificates, got %d",
			numHost, numHost, numCert)
	}

	// Load dcrwallet RPC certificate(s).
	cfg.WalletCerts2 = make([][]byte, numCert)
	for i := 0; i < numCert; i++ {
		certs[i] = cleanAndExpandPath(certs[i])
		cfg.WalletCerts2[i], err = os.ReadFile(certs[i])
		if err != nil {
			return nil, fmt.Errorf("failed to read dcrwallet cert file: %w", err)
		}
	}

	// Verify minimum number of voting wallets are configured.
	if numHost < cfg.Network.MinWallets {
		return nil, fmt.Errorf("minimum required voting wallets has not been met: %d < %d",
			numHost, cfg.Network.MinWallets)
	}

	// Add default port for the active network if there is no port specified.
	for i := 0; i < numHost; i++ {
		cfg.WalletHosts2[i] = normalizeAddress(cfg.WalletHosts2[i], cfg.Network.WalletRPCServerPort)
	}
	cfg.DcrdHost = normalizeAddress(cfg.DcrdHost, cfg.Network.DcrdRPCServerPort)

	// Set the log path.
	cfg.LogPath = filepath.Join(cfg.HomeDir, "logs", cfg.Network.Name)

	// Set the database path.
	dataDir := filepath.Join(cfg.HomeDir, "data", cfg.Network.Name)
	cfg.DbPath = filepath.Join(dataDir, dbFilename)

	// If database does not exist, return error.
	if !fileExists(cfg.DbPath) {
		return nil, fmt.Errorf("no database exists in %s. A new database can"+
			" be created with vspadmin", dataDir)
	}

	return &cfg, nil
}
