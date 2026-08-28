// Copyright (c) 2020-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package vspd

import (
	"context"
	"errors"
	"time"

	"github.com/decred/dcrd/wire"
	"github.com/decred/slog"
	"github.com/decred/vspd/database"
	"github.com/decred/vspd/internal/config"
	"github.com/decred/vspd/rpc"
)

const (
	// consistencyInterval is the time period between wallet consistency checks.
	consistencyInterval = 30 * time.Minute

	// dcrdInterval is the time period between dcrd connection checks.
	dcrdInterval = time.Second * 15
)

type Vspd struct {
	network *config.Network
	log     slog.Logger
	db      *database.VspDatabase
	dcrd    rpc.DcrdConnect
	wallets rpc.WalletConnect

	blockNotifChan chan *wire.BlockHeader

	// lastScannedBlock is the height of the most recent block which has been
	// scanned for spent tickets.
	lastScannedBlock int64
}

func New(network *config.Network, log slog.Logger, db *database.VspDatabase,
	dcrd rpc.DcrdConnect, wallets rpc.WalletConnect, blockNotifChan chan *wire.BlockHeader) *Vspd {

	v := &Vspd{
		network: network,
		log:     log,
		db:      db,
		dcrd:    dcrd,
		wallets: wallets,

		blockNotifChan: blockNotifChan,
	}

	return v
}

func (v *Vspd) Run(ctx context.Context) {
	// Run database integrity checks to ensure all data in database is present
	// and up-to-date.
	err := v.checkDatabaseIntegrity(ctx)
	if err != nil {
		// Don't log error if shutdown was requested, just return.
		if errors.Is(err, context.Canceled) {
			return
		}

		// vspd should still start if this fails, so just log an error.
		v.log.Errorf("Database integrity check failed: %v", err)
	}

	// Stop if shutdown requested.
	if ctx.Err() != nil {
		return
	}

	// Run the update function now to catch up with any blocks mined while vspd
	// was shut down.
	v.update(ctx)

	// Stop if shutdown requested.
	if ctx.Err() != nil {
		return
	}

	// Run voting wallet consistency check now to ensure all wallets are up to
	// date.
	v.checkWalletConsistency(ctx)

	// Stop if shutdown requested.
	if ctx.Err() != nil {
		return
	}

	// Start all background tasks and notification handlers.
	consistencyTicker := time.NewTicker(consistencyInterval)
	defer consistencyTicker.Stop()
	dcrdTicker := time.NewTicker(dcrdInterval)
	defer dcrdTicker.Stop()

	for {
		select {
		// Run voting wallet consistency check periodically.
		case <-consistencyTicker.C:
			v.checkWalletConsistency(ctx)

		// Ensure dcrd client is connected so notifications are received.
		case <-dcrdTicker.C:
			_, _, err := v.dcrd.Client()
			if err != nil {
				v.log.Error(err)
			}

		// Run the update function every time a block connected notification is
		// received from dcrd.
		case header := <-v.blockNotifChan:
			v.log.Debugf("Block notification %d (%s)", header.Height, header.BlockHash().String())
			v.update(ctx)

		// Handle shutdown request.
		case <-ctx.Done():
			return
		}
	}
}
