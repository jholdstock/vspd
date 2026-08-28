// Copyright (c) 2020-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package config

import (
	"fmt"

	"github.com/decred/dcrd/chaincfg/v3"
)

type Network struct {
	*chaincfg.Params
	DcrdRPCServerPort   string
	WalletRPCServerPort string
	BlockExplorerURL    string
	// MinWallets is the minimum number of voting wallets required for a vspd
	// deployment on this network. vspd will log an error and refuse to start if
	// fewer wallets are configured.
	MinWallets int
	// RequiredConfs is the number of confirmations required to consider a
	// ticket purchase or a fee transaction to be final.
	RequiredConfs int64
	// DCP0005Height is the activation height of DCP-0005 block header
	// commitments agenda on this network.
	DCP0005Height int64
	// DCP0010Height is the activation height of DCP-0010 change PoW/PoS subsidy
	// split agenda on this network.
	DCP0010Height int64
	// DCP0012Height is the activation height of DCP-0012 change PoW/PoS subsidy
	// split R2 agenda on this network.
	DCP0012Height int64
}

var MainNet = Network{
	Params:              chaincfg.MainNetParams(),
	DcrdRPCServerPort:   "9109",
	WalletRPCServerPort: "9110",
	BlockExplorerURL:    "https://dcrdata.decred.org",
	MinWallets:          3,
	RequiredConfs:       6,
	// DCP0005Height on mainnet is block
	// 000000000000000010815bed2c4dc431c34a859f4fc70774223dde788e95a01e.
	DCP0005Height: 431488,
	// DCP0010Height on mainnet is block
	// 00000000000000002f4c6aaf0e9cb4d5a74c238d9bf8b8909e2372776c7c214c.
	DCP0010Height: 657280,
	// DCP0012Height on mainnet is block
	// 071683030010299ab13f139df59dc98d637957b766e47f8da6dd5ac762f1e8c7.
	DCP0012Height: 794368,
}

var TestNet3 = Network{
	Params:              chaincfg.TestNet3Params(),
	DcrdRPCServerPort:   "19109",
	WalletRPCServerPort: "19110",
	BlockExplorerURL:    "https://testnet.dcrdata.org",
	MinWallets:          1,
	RequiredConfs:       2,
	// DCP0005Height on testnet3 is block
	// 0000003e54421d585f4a609393a8694509af98f62b8449f245b09fe1389f8f77.
	DCP0005Height: 323328,
	// DCP0010Height on testnet3 is block
	// 000000000000c7fd75f2234bbff6bb81de3a9ebbd2fdd383ae3dbc6205ffe4ff.
	DCP0010Height: 877728,
	// DCP0012Height on testnet3 is block
	// c7da7b548a2a9463dc97adb48433c4ffff18c3873f7e2ae99338a990dae039f0.
	DCP0012Height: 1170048,
}

var SimNet = Network{
	Params:              chaincfg.SimNetParams(),
	DcrdRPCServerPort:   "19556",
	WalletRPCServerPort: "19557",
	BlockExplorerURL:    "...",
	MinWallets:          1,
	RequiredConfs:       2,
	// DCP0005Height on simnet is 1 because the agenda will always be active.
	DCP0005Height: 1,
	// DCP0010Height on simnet is 1 because the agenda will always be active.
	DCP0010Height: 1,
	// DCP0012Height on simnet is 1 because the agenda will always be active.
	DCP0012Height: 1,
}

func NetworkFromName(name string) (*Network, error) {
	switch name {
	case "mainnet":
		return &MainNet, nil
	case "testnet":
		return &TestNet3, nil
	case "simnet":
		return &SimNet, nil
	default:
		return nil, fmt.Errorf("%q is not a supported network", name)
	}
}

// DCP5Active returns true if the DCP-0005 block header commitments agenda is
// active on this network at the provided height, otherwise false.
func (n *Network) DCP5Active(height int64) bool {
	return height >= n.DCP0005Height
}

// DCP10Active returns true if the DCP-0010 change PoW/PoS subsidy split agenda
// is active on this network at the provided height, otherwise false.
func (n *Network) DCP10Active(height int64) bool {
	return height >= n.DCP0010Height
}

// DCP12Active returns true if the DCP-0012 change PoW/PoS subsidy split R2
// agenda is active on this network at the provided height, otherwise false.
func (n *Network) DCP12Active(height int64) bool {
	return height >= n.DCP0012Height
}

// CurrentVoteVersion returns the most recent version in the current networks
// consensus agenda deployments.
func (n *Network) CurrentVoteVersion() uint32 {
	var latestVersion uint32
	for version := range n.Deployments {
		if latestVersion < version {
			latestVersion = version
		}
	}
	return latestVersion
}
