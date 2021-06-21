// Copyright (c) 2020-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package webapi

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/decred/dcrd/blockchain/v4"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/vspd/database"
	"github.com/decred/vspd/rpc"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

// payFee is the handler for "POST /api/v3/payfee".
func payFee(c *gin.Context) {
	const funcName = "payFee"

	// Get values which have been added to context by middleware.
	ticket := c.MustGet("Ticket").(database.Ticket)
	knownTicket := c.MustGet("KnownTicket").(bool)
	dcrdClient := c.MustGet("DcrdClient").(*rpc.DcrdRPC)
	reqBytes := c.MustGet("RequestBytes").([]byte)

	if cfg.VspClosed {
		sendError(errVspClosed, c)
		return
	}

	if !knownTicket {
		log.Warnf("%s: Unknown ticket (clientIP=%s)", funcName, c.ClientIP())
		sendError(errUnknownTicket, c)
		return
	}

	var request payFeeRequest
	if err := binding.JSON.BindBody(reqBytes, &request); err != nil {
		log.Warnf("%s: Bad request (clientIP=%s): %v", funcName, c.ClientIP(), err)
		sendErrorWithMsg(err.Error(), errBadRequest, c)
		return
	}

	// Respond early if we already have the fee tx for this ticket.
	if ticket.FeeTxStatus == database.FeeReceieved ||
		ticket.FeeTxStatus == database.FeeBroadcast ||
		ticket.FeeTxStatus == database.FeeConfirmed {
		log.Warnf("%s: Fee tx already received (clientIP=%s, ticketHash=%s)",
			funcName, c.ClientIP(), ticket.Hash)
		sendError(errFeeAlreadyReceived, c)
		return
	}

	// Get ticket details.
	rawTicket, err := dcrdClient.GetRawTransaction(ticket.Hash)
	if err != nil {
		log.Errorf("%s: dcrd.GetRawTransaction for ticket failed (ticketHash=%s): %v", funcName, ticket.Hash, err)
		sendError(errInternalError, c)
		return
	}

	// Ensure this ticket is eligible to vote at some point in the future.
	canVote, err := dcrdClient.CanTicketVote(rawTicket, ticket.Hash, cfg.NetParams)
	if err != nil {
		log.Errorf("%s: dcrd.CanTicketVote error (ticketHash=%s): %v", funcName, ticket.Hash, err)
		sendError(errInternalError, c)
		return
	}
	if !canVote {
		log.Warnf("%s: Unvotable ticket (clientIP=%s, ticketHash=%s)",
			funcName, c.ClientIP(), ticket.Hash)
		sendError(errTicketCannotVote, c)
		return
	}

	// Respond early if the fee for this ticket is expired.
	if ticket.FeeExpired() {
		log.Warnf("%s: Expired payfee request (clientIP=%s, ticketHash=%s)",
			funcName, c.ClientIP(), ticket.Hash)
		sendError(errFeeExpired, c)
		return
	}

	// Validate VotingKey.
	votingKey := request.VotingKey
	votingWIF, err := dcrutil.DecodeWIF(votingKey, cfg.NetParams.PrivateKeyID)
	if err != nil {
		log.Warnf("%s: Failed to decode WIF (clientIP=%s, ticketHash=%s): %v",
			funcName, c.ClientIP(), ticket.Hash, err)
		sendError(errInvalidPrivKey, c)
		return
	}

	// Validate VoteChoices. Just log a warning if vote choices are not valid
	// for the current vote version - the ticket should still be registered.
	validVoteChoices := true
	err = validConsensusVoteChoices(cfg.NetParams, currentVoteVersion(cfg.NetParams), request.VoteChoices)
	if err != nil {
		validVoteChoices = false
		log.Warnf("%s: Invalid consensus vote choices (clientIP=%s, ticketHash=%s): %v",
			funcName, c.ClientIP(), ticket.Hash, err)
	}

	validTreasuryVote := true
	err = validTreasuryPolicy(request.TreasuryPolicy)
	if err != nil {
		validTreasuryVote = false
		log.Warnf("%s: Invalid treasury policy (clientIP=%s, ticketHash=%s): %v",
			funcName, c.ClientIP(), ticket.Hash, err)
	}

	validTSpendVote := true
	err = validTSpendPolicy(request.TSpendPolicy)
	if err != nil {
		validTSpendVote = false
		log.Warnf("%s: Invalid tspend policy (clientIP=%s, ticketHash=%s): %v",
			funcName, c.ClientIP(), ticket.Hash, err)
	}

	// Validate FeeTx.
	feeTx, err := decodeTransaction(request.FeeTx)
	if err != nil {
		log.Warnf("%s: Failed to decode fee tx hex (clientIP=%s, ticketHash=%s): %v",
			funcName, c.ClientIP(), ticket.Hash, err)
		sendError(errInvalidFeeTx, c)
		return
	}

	err = blockchain.CheckTransactionSanity(feeTx, cfg.NetParams)
	if err != nil {
		log.Warnf("%s: Fee tx failed sanity check (clientIP=%s, ticketHash=%s): %v",
			funcName, c.ClientIP(), ticket.Hash, err)
		sendError(errInvalidFeeTx, c)
		return
	}

	// Decode fee address to get its payment script details.
	feeAddr, err := stdaddr.DecodeAddress(ticket.FeeAddress, cfg.NetParams)
	if err != nil {
		log.Errorf("%s: Failed to decode fee address (ticketHash=%s): %v",
			funcName, ticket.Hash, err)
		sendError(errInternalError, c)
		return
	}

	wantScriptVer, wantScript := feeAddr.PaymentScript()

	// Confirm the provided fee transaction contains an output which pays to the
	// expected payment script. Both script and script version should match.
	var feePaid dcrutil.Amount
	for _, txOut := range feeTx.TxOut {
		if txOut.Version == wantScriptVer && bytes.Equal(txOut.PkScript, wantScript) {
			feePaid = dcrutil.Amount(txOut.Value)
			break
		}
	}

	// Confirm a fee payment was found.
	if feePaid == 0 {
		log.Warnf("%s: Fee tx did not include expected payment (ticketHash=%s, feeAddress=%s, clientIP=%s)",
			funcName, ticket.Hash, ticket.FeeAddress, c.ClientIP())
		sendErrorWithMsg(
			fmt.Sprintf("feetx did not include any payments for fee address %s", ticket.FeeAddress),
			errInvalidFeeTx, c)
		return
	}

	// Confirm fee payment is equal to or larger than the minimum expected.
	minFee := dcrutil.Amount(ticket.FeeAmount)
	if feePaid < minFee {
		log.Warnf("%s: Fee too small (ticketHash=%s, clientIP=%s): was %s, expected minimum %s",
			funcName, ticket.Hash, c.ClientIP(), feePaid, minFee)
		sendError(errFeeTooSmall, c)
		return
	}

	// Decode the provided voting WIF to get its voting rights script.
	pkHash := stdaddr.Hash160(votingWIF.PubKey())
	wifAddr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(pkHash, cfg.NetParams)
	if err != nil {
		log.Errorf("%s: Failed to get voting address from WIF (ticketHash=%s, clientIP=%s): %v",
			funcName, ticket.Hash, c.ClientIP(), err)
		sendError(errInvalidPrivKey, c)
		return
	}

	wantScriptVer, wantScript = wifAddr.VotingRightsScript()

	// Decode ticket transaction to get its voting rights script.
	ticketTx, err := decodeTransaction(rawTicket.Hex)
	if err != nil {
		log.Warnf("%s: Failed to decode ticket hex (ticketHash=%s): %v",
			funcName, ticket.Hash, err)
		sendError(errInternalError, c)
		return
	}

	actualScriptVer := ticketTx.TxOut[0].Version
	actualScript := ticketTx.TxOut[0].PkScript

	// Ensure provided voting WIF matches the actual voting address of the
	// ticket. Both script and script version should match.
	if actualScriptVer != wantScriptVer || !bytes.Equal(actualScript, wantScript) {
		log.Warnf("%s: Voting address does not match provided private key: (ticketHash=%s)",
			funcName, ticket.Hash)
		sendErrorWithMsg("voting address does not match provided private key",
			errInvalidPrivKey, c)
		return
	}

	// At this point we are satisfied that the request is valid and the fee tx
	// pays sufficient fees to the expected address. Proceed to update the
	// database, and if the ticket is confirmed broadcast the fee transaction.

	ticket.VotingWIF = votingWIF.String()
	ticket.FeeTxHex = request.FeeTx
	ticket.FeeTxHash = feeTx.TxHash().String()
	ticket.FeeTxStatus = database.FeeReceieved

	if validVoteChoices {
		ticket.VoteChoices = request.VoteChoices
	}

	if validTSpendVote {
		ticket.TSpendPolicy = request.TSpendPolicy
	}

	if validTreasuryVote {
		ticket.TreasuryPolicy = request.TreasuryPolicy
	}

	err = db.UpdateTicket(ticket)
	if err != nil {
		log.Errorf("%s: db.UpdateTicket error, failed to set fee tx (ticketHash=%s): %v",
			funcName, ticket.Hash, err)
		sendError(errInternalError, c)
		return
	}

	log.Debugf("%s: Fee tx received for ticket (minExpectedFee=%v, feePaid=%v, ticketHash=%s)",
		funcName, minFee, feePaid, ticket.Hash)

	if ticket.Confirmed {
		err = dcrdClient.SendRawTransaction(request.FeeTx)
		if err != nil {
			log.Errorf("%s: dcrd.SendRawTransaction for fee tx failed (ticketHash=%s): %v",
				funcName, ticket.Hash, err)

			ticket.FeeTxStatus = database.FeeError

			if strings.Contains(err.Error(),
				"references outputs of unknown or fully-spent transaction") {
				sendError(errCannotBroadcastFeeUnknownOutputs, c)
			} else {
				sendError(errCannotBroadcastFee, c)
			}

			err = db.UpdateTicket(ticket)
			if err != nil {
				log.Errorf("%s: db.UpdateTicket error, failed to set fee tx error (ticketHash=%s): %v",
					funcName, ticket.Hash, err)
			}

			return
		}

		ticket.FeeTxStatus = database.FeeBroadcast

		err = db.UpdateTicket(ticket)
		if err != nil {
			log.Errorf("%s: db.UpdateTicket error, failed to set fee tx as broadcast (ticketHash=%s): %v",
				funcName, ticket.Hash, err)
			sendError(errInternalError, c)
			return
		}

		log.Debugf("%s: Fee tx broadcast for ticket (ticketHash=%s, feeHash=%s)",
			funcName, ticket.Hash, ticket.FeeTxHash)
	}

	// Send success response to client.
	resp, respSig := sendJSONResponse(payFeeResponse{
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
