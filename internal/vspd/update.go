// Copyright (c) 2020-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package vspd

import (
	"context"
	"errors"
	"strings"

	"github.com/decred/vspd/database"
	"github.com/decred/vspd/rpc"
	"github.com/jrick/wsrpc/v2"
)

// update uses the latest available information from dcrd to update all of the
// data in the vspd database. When appropriate it will also broadcast pending
// fee transactions and add tickets to voting wallets.
func (v *Vspd) update(ctx context.Context) {
	const funcName = "update"

	dcrdClient, _, err := v.dcrd.Client()
	if err != nil {
		v.log.Errorf("%s: %v", funcName, err)
		return
	}

	// Step 1/4: Update the database with any tickets which now have 6+
	// confirmations.
	v.updateUnconfirmed(ctx, dcrdClient)
	if ctx.Err() != nil {
		return
	}

	// Step 2/4: Broadcast fee tx for tickets which are confirmed.
	v.broadcastFees(ctx, dcrdClient)
	if ctx.Err() != nil {
		return
	}

	// Step 3/4: Add tickets with confirmed fees to voting wallets.
	v.addToWallets(ctx, dcrdClient)
	if ctx.Err() != nil {
		return
	}

	// Step 4/4: Set ticket outcome in database if any tickets are
	// voted/revoked.
	v.setOutcomes(ctx, dcrdClient)
	if ctx.Err() != nil {
		return
	}
}

func (v *Vspd) updateUnconfirmed(ctx context.Context, dcrdClient *rpc.DcrdRPC) {
	const funcName = "updateUnconfirmed"

	unconfirmed, err := v.db.GetUnconfirmedTickets()
	if err != nil {
		v.log.Errorf("%s: db.GetUnconfirmedTickets error: %v", funcName, err)
		return
	}

	for _, ticket := range unconfirmed {
		// Exit early if context has been canceled.
		if ctx.Err() != nil {
			return
		}

		tktTx, err := dcrdClient.GetRawTransaction(ticket.Hash)
		if err != nil {
			// ErrNoTxInfo here probably indicates a tx which was never mined
			// and has been removed from the mempool. For example, a ticket
			// purchase tx close to an sdiff change, or a ticket purchase tx
			// which expired. Remove it from the db.
			var e *wsrpc.Error
			if errors.As(err, &e) && e.Code == rpc.ErrNoTxInfo {
				v.log.Infof("Removing unconfirmed ticket from db - no information available "+
					"about transaction (ticketHash=%s)", ticket.Hash)

				err = v.db.DeleteTicket(ticket)
				if err != nil {
					v.log.Errorf("%s: db.DeleteTicket error (ticketHash=%s): %v",
						funcName, ticket.Hash, err)
				}

				err = v.db.DeleteVoteChanges(ticket.Hash)
				if err != nil {
					v.log.Errorf("%s: db.DeleteVoteChanges error (ticketHash=%s): %v",
						funcName, ticket.Hash, err)
				}

				// This will not error if an alternate signing address does not
				// exist for ticket.
				err = v.db.DeleteAltSignAddr(ticket.Hash)
				if err != nil {
					v.log.Errorf("%s: db.DeleteAltSignAddr error (ticketHash=%s): %v",
						funcName, ticket.Hash, err)
				}
			} else {
				v.log.Errorf("%s: dcrd.GetRawTransaction for ticket failed (ticketHash=%s): %v",
					funcName, ticket.Hash, err)
			}

			continue
		}

		if tktTx.Confirmations >= v.network.RequiredConfs {
			ticket.PurchaseHeight = tktTx.BlockHeight
			ticket.Confirmed = true
			err = v.db.UpdateTicket(ticket)
			if err != nil {
				v.log.Errorf("%s: db.UpdateTicket error, failed to set ticket as confirmed (ticketHash=%s): %v",
					funcName, ticket.Hash, err)
				continue
			}

			v.log.Infof("Ticket confirmed (ticketHash=%s)", ticket.Hash)
		}
	}
}

func (v *Vspd) broadcastFees(ctx context.Context, dcrdClient *rpc.DcrdRPC) {
	const funcName = "broadcastFees"

	pending, err := v.db.GetPendingFees()
	if err != nil {
		v.log.Errorf("%s: db.GetPendingFees error: %v", funcName, err)
		return
	}

	for _, ticket := range pending {
		// Exit early if context has been canceled.
		if ctx.Err() != nil {
			return
		}

		err = dcrdClient.SendRawTransaction(ticket.FeeTxHex)
		if err != nil {
			v.log.Errorf("%s: dcrd.SendRawTransaction for fee tx failed (ticketHash=%s): %v",
				funcName, ticket.Hash, err)
			ticket.FeeTxStatus = database.FeeError
		} else {
			v.log.Infof("Fee tx broadcast for ticket (ticketHash=%s, feeHash=%s)",
				ticket.Hash, ticket.FeeTxHash)
			ticket.FeeTxStatus = database.FeeBroadcast
		}

		err = v.db.UpdateTicket(ticket)
		if err != nil {
			v.log.Errorf("%s: db.UpdateTicket error, failed to set fee tx as broadcast (ticketHash=%s): %v",
				funcName, ticket.Hash, err)
		}
	}
}

func (v *Vspd) addToWallets(ctx context.Context, dcrdClient *rpc.DcrdRPC) {
	const funcName = "addToWallets"

	unconfirmedFees, err := v.db.GetUnconfirmedFees()
	if err != nil {
		v.log.Errorf("%s: db.GetUnconfirmedFees error: %v", funcName, err)
		return
	}

	walletClients, failedConnections := v.wallets.Clients()
	if len(walletClients) == 0 {
		v.log.Errorf("%s: Could not connect to any wallets", funcName)
		return
	}
	if len(failedConnections) > 0 {
		v.log.Errorf("%s: Failed to connect to %d wallet(s), proceeding with only %d",
			funcName, len(failedConnections), len(walletClients))
	}

	for _, ticket := range unconfirmedFees {
		// Exit early if context has been canceled.
		if ctx.Err() != nil {
			return
		}

		feeTx, err := dcrdClient.GetRawTransaction(ticket.FeeTxHash)
		if err != nil {
			v.log.Errorf("%s: dcrd.GetRawTransaction for fee tx failed (feeTxHash=%s, ticketHash=%s): %v",
				funcName, ticket.FeeTxHash, ticket.Hash, err)

			ticket.FeeTxStatus = database.FeeError
			err = v.db.UpdateTicket(ticket)
			if err != nil {
				v.log.Errorf("%s: db.UpdateTicket error, failed to set fee tx status to error (ticketHash=%s): %v",
					funcName, ticket.Hash, err)
			}
			continue
		}

		// If fee is confirmed, update the database and add ticket to voting
		// wallets.
		if feeTx.Confirmations >= v.network.RequiredConfs {
			// We no longer need the hex once the tx is confirmed on-chain.
			ticket.FeeTxHex = ""
			ticket.FeeTxStatus = database.FeeConfirmed
			err = v.db.UpdateTicket(ticket)
			if err != nil {
				v.log.Errorf("%s: db.UpdateTicket error, failed to set fee tx as confirmed (ticketHash=%s): %v",
					funcName, ticket.Hash, err)
				continue
			}
			v.log.Infof("Fee tx confirmed (ticketHash=%s)", ticket.Hash)

			// Add ticket to the voting wallet.

			rawTicket, err := dcrdClient.GetRawTransaction(ticket.Hash)
			if err != nil {
				v.log.Errorf("%s: dcrd.GetRawTransaction for ticket failed (ticketHash=%s): %v",
					funcName, ticket.Hash, err)
				continue
			}

			// Count how many wallets the ticket is added to for logging.
			added := 0
			for _, walletClient := range walletClients {
				err = walletClient.AddTicketForVoting(ticket.VotingWIF, rawTicket.BlockHash, rawTicket.Hex)
				if err != nil {
					v.log.Errorf("%s: dcrwallet.AddTicketForVoting error (wallet=%s, ticketHash=%s): %v",
						funcName, walletClient.String(), ticket.Hash, err)
					continue
				}
				added++

				// Set consensus vote choices on voting wallets.
				for agenda, choice := range ticket.VoteChoices {
					err = walletClient.SetVoteChoice(agenda, choice, ticket.Hash)
					if err != nil {
						if strings.Contains(err.Error(), "no agenda with ID") {
							v.log.Warnf("%s: Removing invalid agenda from ticket vote choices (ticketHash=%s, agenda=%s)",
								funcName, ticket.Hash, agenda)
							delete(ticket.VoteChoices, agenda)
							err = v.db.UpdateTicket(ticket)
							if err != nil {
								v.log.Errorf("%s: db.UpdateTicket error, failed to remove invalid agenda (ticketHash=%s): %v",
									funcName, ticket.Hash, err)
							}
						} else {
							v.log.Errorf("%s: dcrwallet.SetVoteChoice error (wallet=%s, ticketHash=%s): %v",
								funcName, walletClient.String(), ticket.Hash, err)
						}
					}
				}

				// Set tspend policy on voting wallets.
				for tspend, policy := range ticket.TSpendPolicy {
					err = walletClient.SetTSpendPolicy(tspend, policy, ticket.Hash)
					if err != nil {
						v.log.Errorf("%s: dcrwallet.SetTSpendPolicy failed (wallet=%s, ticketHash=%s): %v",
							funcName, walletClient.String(), ticket.Hash, err)
					}
				}

				// Set treasury policy on voting wallets.
				for key, policy := range ticket.TreasuryPolicy {
					err = walletClient.SetTreasuryPolicy(key, policy, ticket.Hash)
					if err != nil {
						v.log.Errorf("%s: dcrwallet.SetTreasuryPolicy failed (wallet=%s, ticketHash=%s): %v",
							funcName, walletClient.String(), ticket.Hash, err)
					}
				}
			}

			v.log.Infof("Ticket added to %s (ticketHash=%s)",
				pluralize(added, "voting wallet"),
				ticket.Hash)
		}
	}
}

func (v *Vspd) setOutcomes(ctx context.Context, dcrdClient *rpc.DcrdRPC) {
	const funcName = "setOutcomes"

	votableTickets, err := v.db.GetVotableTickets()
	if err != nil {
		v.log.Errorf("%s: db.GetVotableTickets failed: %v", funcName, err)
		return
	}

	// If the database has no votable tickets, there is nothing more to do.
	if len(votableTickets) == 0 {
		return
	}

	var startHeight int64
	if v.lastScannedBlock == 0 {
		// Use the earliest height at which a votable ticket matured if vspd has
		// not performed a scan for spent tickets since it started. This will
		// catch any tickets which were spent whilst vspd was offline.
		startHeight = votableTickets.EarliestPurchaseHeight() + int64(v.network.TicketMaturity)
	} else {
		startHeight = v.lastScannedBlock
	}

	spent, endHeight, err := v.findSpentTickets(ctx, dcrdClient, votableTickets, startHeight)
	if err != nil {
		// Don't log error if shutdown was requested, just return.
		if errors.Is(err, context.Canceled) {
			return
		}

		v.log.Errorf("%s: findSpentTickets error: %v", funcName, err)
		return
	}

	v.lastScannedBlock = endHeight

	for _, spentTicket := range spent {
		// Exit early if context has been canceled.
		if ctx.Err() != nil {
			return
		}

		dbTicket := spentTicket.dbTicket

		switch {
		case spentTicket.voted():
			dbTicket.Outcome = database.Voted
		case spentTicket.missed():
			dbTicket.Outcome = database.Missed
		default:
			dbTicket.Outcome = database.Expired
		}

		err = v.db.UpdateTicket(dbTicket)
		if err != nil {
			v.log.Errorf("%s: db.UpdateTicket error, failed to set ticket outcome (ticketHash=%s): %v",
				funcName, dbTicket.Hash, err)
			continue
		}

		v.log.Infof("Ticket %s at height %d (ticketHash=%s)",
			dbTicket.Outcome, spentTicket.heightSpent, dbTicket.Hash)
	}
}
