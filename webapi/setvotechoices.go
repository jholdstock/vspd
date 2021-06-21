// Copyright (c) 2020-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package webapi

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/decred/vspd/database"
	"github.com/decred/vspd/rpc"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

// setVoteChoices is the handler for "POST /api/v3/setvotechoices".
func setVoteChoices(c *gin.Context) {
	const funcName = "setVoteChoices"

	// Get values which have been added to context by middleware.
	ticket := c.MustGet("Ticket").(database.Ticket)
	knownTicket := c.MustGet("KnownTicket").(bool)
	walletClients := c.MustGet("WalletClients").([]*rpc.WalletRPC)
	reqBytes := c.MustGet("RequestBytes").([]byte)

	// If we cannot set the vote choices on at least one voting wallet right
	// now, don't update the database, just return an error.
	if len(walletClients) == 0 {
		sendError(errInternalError, c)
		return
	}

	if !knownTicket {
		log.Warnf("%s: Unknown ticket (clientIP=%s)", funcName, c.ClientIP())
		sendError(errUnknownTicket, c)
		return
	}

	if ticket.FeeTxStatus == database.NoFee {
		log.Warnf("%s: No fee tx for ticket (clientIP=%s, ticketHash=%s)",
			funcName, c.ClientIP(), ticket.Hash)
		sendError(errFeeNotReceived, c)
		return
	}

	// Only allow vote choices to be updated for live/immature tickets.
	if ticket.Outcome != "" {
		log.Warnf("%s: Ticket not eligible to vote (clientIP=%s, ticketHash=%s)",
			funcName, c.ClientIP(), ticket.Hash)
		sendErrorWithMsg(fmt.Sprintf("ticket not eligible to vote (status=%s)", ticket.Outcome),
			errTicketCannotVote, c)
		return
	}

	var request setVoteChoicesRequest
	if err := binding.JSON.BindBody(reqBytes, &request); err != nil {
		log.Warnf("%s: Bad request (clientIP=%s): %v", funcName, c.ClientIP(), err)
		sendErrorWithMsg(err.Error(), errBadRequest, c)
		return
	}

	// Return an error if this request has a timestamp older than any previous
	// vote change requests. This is to prevent requests from being replayed.
	previousChanges, err := db.GetVoteChanges(ticket.Hash)
	if err != nil {
		log.Errorf("%s: db.GetVoteChanges error (ticketHash=%s): %v",
			funcName, ticket.Hash, err)
		sendError(errInternalError, c)
		return
	}

	for _, change := range previousChanges {
		var prevReq struct {
			Timestamp int64 `json:"timestamp" binding:"required"`
		}
		err := json.Unmarshal([]byte(change.Request), &prevReq)
		if err != nil {
			log.Errorf("%s: Could not unmarshal vote change record (ticketHash=%s): %v",
				funcName, ticket.Hash, err)
			sendError(errInternalError, c)
			return
		}

		if request.Timestamp <= prevReq.Timestamp {
			log.Warnf("%s: Request uses invalid timestamp, %d is not greater "+
				"than %d (ticketHash=%s)",
				funcName, request.Timestamp, prevReq.Timestamp, ticket.Hash)
			sendError(errInvalidTimestamp, c)
			return
		}
	}

	// Validate vote choices (consensus, tspend policy and treasury policy).

	err = validConsensusVoteChoices(cfg.NetParams, currentVoteVersion(cfg.NetParams), request.VoteChoices)
	if err != nil {
		log.Warnf("%s: Invalid consensus vote choices (clientIP=%s, ticketHash=%s): %v",
			funcName, c.ClientIP(), ticket.Hash, err)
		sendErrorWithMsg(err.Error(), errInvalidVoteChoices, c)
		return
	}

	err = validTreasuryPolicy(request.TreasuryPolicy)
	if err != nil {
		log.Warnf("%s: Invalid treasury policy (clientIP=%s, ticketHash=%s): %v",
			funcName, c.ClientIP(), ticket.Hash, err)
		sendErrorWithMsg(err.Error(), errInvalidVoteChoices, c)
	}

	err = validTSpendPolicy(request.TSpendPolicy)
	if err != nil {
		log.Warnf("%s: Invalid tspend policy (clientIP=%s, ticketHash=%s): %v",
			funcName, c.ClientIP(), ticket.Hash, err)
		sendErrorWithMsg(err.Error(), errInvalidVoteChoices, c)
	}

	// TSpendPolicy is optional, so only run this if provided.
	var tSpendToDelete []string
	if len(request.TSpendPolicy) > 0 {
		// Find any TSpendPolicies which need to be removed from voting wallets
		// - i.e. any policies which are set in the database but are not
		// included in the request.
		for k := range ticket.TSpendPolicy {
			if _, ok := request.TSpendPolicy[k]; !ok {
				tSpendToDelete = append(tSpendToDelete, k)
			}
		}

		ticket.TSpendPolicy = request.TSpendPolicy
	}

	// TreasuryPolicy is optional, so only run this if provided.
	var treasuryToDelete []string
	if len(request.TreasuryPolicy) > 0 {
		// Find any TreasuryPolicies which need to be removed from voting wallets
		// - i.e. any policies which are set in the database but are not
		// included in the request.
		for k := range ticket.TreasuryPolicy {
			if _, ok := request.TreasuryPolicy[k]; !ok {
				treasuryToDelete = append(treasuryToDelete, k)
			}
		}

		ticket.TreasuryPolicy = request.TreasuryPolicy
	}

	// Update VoteChoices in the database before updating the wallets. DB is the
	// source of truth, and also is less likely to error.
	ticket.VoteChoices = request.VoteChoices

	err = db.UpdateTicket(ticket)
	if err != nil {
		log.Errorf("%s: db.UpdateTicket error, failed to set vote choices (ticketHash=%s): %v",
			funcName, ticket.Hash, err)
		sendError(errInternalError, c)
		return
	}

	// Update vote choices on voting wallets. Tickets are only added to voting
	// wallets if their fee is confirmed.
	if ticket.FeeTxStatus == database.FeeConfirmed {

		// Just log any errors which occur while setting vote choices. We want
		// to attempt to update as much as possible regardless of any errors.
		for _, walletClient := range walletClients {

			// Consensus vote choices.
			for agenda, choice := range request.VoteChoices {
				err = walletClient.SetVoteChoice(agenda, choice, ticket.Hash)
				if err != nil {
					log.Errorf("%s: dcrwallet.SetVoteChoice failed (wallet=%s, ticketHash=%s): %v",
						funcName, walletClient.String(), ticket.Hash, err)
				}
			}

			// Remove any outdated tspend policies.
			for _, tspend := range tSpendToDelete {
				err = walletClient.RemoveTSpendPolicy(tspend, ticket.Hash)
				if err != nil {
					log.Errorf("%s: dcrwallet.RemoveTSpendPolicy failed (wallet=%s, ticketHash=%s): %v",
						funcName, walletClient.String(), ticket.Hash, err)
				}
			}

			// Update tspend policy.
			for tspend, policy := range request.TSpendPolicy {
				err = walletClient.SetTSpendPolicy(tspend, policy, ticket.Hash)
				if err != nil {
					log.Errorf("%s: dcrwallet.SetTSpendPolicy failed (wallet=%s, ticketHash=%s): %v",
						funcName, walletClient.String(), ticket.Hash, err)
				}
			}

			// Remove any outdated treasury policies.
			for _, treasury := range treasuryToDelete {
				err = walletClient.RemoveTreasuryPolicy(treasury, ticket.Hash)
				if err != nil {
					log.Errorf("%s: dcrwallet.RemoveTreasuryPolicy failed (wallet=%s, ticketHash=%s): %v",
						funcName, walletClient.String(), ticket.Hash, err)
				}
			}

			// Update treasury policy.
			for key, policy := range request.TreasuryPolicy {
				err = walletClient.SetTreasuryPolicy(key, policy, ticket.Hash)
				if err != nil {
					log.Errorf("%s: dcrwallet.SetTreasuryPolicy failed (wallet=%s, ticketHash=%s): %v",
						funcName, walletClient.String(), ticket.Hash, err)
				}
			}
		}
	}

	log.Debugf("%s: Vote choices updated (ticketHash=%s)", funcName, ticket.Hash)

	// Send success response to client.
	resp, respSig := sendJSONResponse(setVoteChoicesResponse{
		Timestamp: time.Now().Unix(),
		Request:   reqBytes,
	}, c)

	// Store a record of the vote choice change.
	err = db.SaveVoteChange(
		ticket.Hash,
		database.VoteChangeRecord{
			Request:           string(reqBytes),
			RequestSignature:  c.GetHeader("VSP-Client-Signature"),
			Response:          resp,
			ResponseSignature: respSig,
		})
	if err != nil {
		log.Errorf("%s: Failed to store vote change record (ticketHash=%s): %v", err)
	}
}
