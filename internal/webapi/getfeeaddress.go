// Copyright (c) 2021-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package webapi

import (
	"sync"
	"time"

	"decred.org/dcrwallet/v5/wallet/txrules"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/vspd/database"
	"github.com/decred/vspd/rpc"
	"github.com/decred/vspd/types/v3"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

// addrMtx protects getNewFeeAddress.
var addrMtx sync.Mutex

// getNewFeeAddress gets a new address from the address generator, and updates
// the last used address index in the database. In order to maintain consistency
// between the internal counter of address generator and the database, this func
// uses a mutex to ensure it is not run concurrently.
func (w *WebAPI) getNewFeeAddress() (string, uint32, error) {
	addrMtx.Lock()
	defer addrMtx.Unlock()

	addr, idx, err := w.addrGen.nextAddress()
	if err != nil {
		return "", 0, err
	}

	err = w.db.SetLastAddressIndex(idx)
	if err != nil {
		return "", 0, err
	}

	return addr, idx, nil
}

// getCurrentFee returns the minimum fee amount a client should pay in order to
// register a ticket with the VSP at the current block height.
func (w *WebAPI) getCurrentFee(dcrdClient *rpc.DcrdRPC) (dcrutil.Amount, error) {
	bestBlock, err := dcrdClient.GetBestBlockHeader()
	if err != nil {
		return 0, err
	}

	sDiff := dcrutil.Amount(bestBlock.SBits)

	// Using a hard-coded amount for relay fee is acceptable here because this
	// amount is never actually used to construct or broadcast transactions. It
	// is only used to calculate the fee charged for adding a ticket to the VSP.
	const defaultMinRelayTxFee = dcrutil.Amount(1e4)

	height := int64(bestBlock.Height)
	isDCP0010Active := w.cfg.Network.DCP10Active(height)
	isDCP0012Active := w.cfg.Network.DCP12Active(height)

	fee := txrules.StakePoolTicketFee(sDiff, defaultMinRelayTxFee, int32(bestBlock.Height),
		w.cfg.VSPFee, w.cfg.Network.Params, isDCP0010Active, isDCP0012Active)

	return fee, nil
}

// feeAddress is the handler for "POST /api/v3/feeaddress".
func (w *WebAPI) feeAddress(c *gin.Context) {

	const funcName = "feeAddress"

	// Get values which have been added to context by middleware.
	ticket := c.MustGet(ticketKey).(database.Ticket)
	knownTicket := c.MustGet(knownTicketKey).(bool)
	commitmentAddress := c.MustGet(commitmentAddressKey).(string)
	dcrdClient := c.MustGet(dcrdKey).(*rpc.DcrdRPC)
	dcrdErr := c.MustGet(dcrdErrorKey)
	if dcrdErr != nil {
		w.log.Errorf("%s: %v", funcName, dcrdErr.(error))
		w.sendError(types.ErrInternalError, c)
		return
	}
	reqBytes := c.MustGet(requestBytesKey).([]byte)

	var request types.FeeAddressRequest
	if err := binding.JSON.BindBody(reqBytes, &request); err != nil {
		w.log.Warnf("%s: Bad request (clientIP=%s): %v", funcName, c.ClientIP(), err)
		w.sendErrorWithMsg(err.Error(), types.ErrBadRequest, c)
		return
	}

	ticketHash := request.TicketHash

	// Respond early if we already have the fee tx for this ticket.
	if knownTicket &&
		(ticket.FeeTxStatus == database.FeeReceieved ||
			ticket.FeeTxStatus == database.FeeBroadcast ||
			ticket.FeeTxStatus == database.FeeConfirmed) {
		w.log.Warnf("%s: Fee tx already received (clientIP=%s, ticketHash=%s)",
			funcName, c.ClientIP(), ticket.Hash)
		w.sendError(types.ErrFeeAlreadyReceived, c)
		return
	}

	// Get ticket details.
	rawTicket, err := dcrdClient.GetRawTransaction(ticketHash)
	if err != nil {
		w.log.Errorf("%s: dcrd.GetRawTransaction for ticket failed (ticketHash=%s): %v", funcName, ticketHash, err)
		w.sendError(types.ErrInternalError, c)
		return
	}

	// Ensure this ticket is eligible to vote at some point in the future.
	canVote, err := canTicketVote(rawTicket, dcrdClient, w.cfg.Network)
	if err != nil {
		w.log.Errorf("%s: canTicketVote error (ticketHash=%s): %v", funcName, ticketHash, err)
		w.sendError(types.ErrInternalError, c)
		return
	}
	if !canVote {
		w.log.Warnf("%s: Unvotable ticket (clientIP=%s, ticketHash=%s)",
			funcName, c.ClientIP(), ticketHash)
		w.sendError(types.ErrTicketCannotVote, c)
		return
	}

	// VSP already knows this ticket and has already issued it a fee address.
	if knownTicket {

		// If the expiry period has passed we need to issue a new fee.
		now := time.Now()
		if ticket.FeeExpired() {
			newFee, err := w.getCurrentFee(dcrdClient)
			if err != nil {
				w.log.Errorf("%s: getCurrentFee error (ticketHash=%s): %v", funcName, ticket.Hash, err)
				w.sendError(types.ErrInternalError, c)
				return
			}
			ticket.FeeExpiration = now.Add(feeAddressExpiration).Unix()
			ticket.FeeAmount = int64(newFee)

			err = w.db.UpdateTicket(ticket)
			if err != nil {
				w.log.Errorf("%s: db.UpdateTicket error, failed to update fee expiry (ticketHash=%s): %v",
					funcName, ticket.Hash, err)
				w.sendError(types.ErrInternalError, c)
				return
			}
			w.log.Debugf("%s: Expired fee updated (newFeeAmt=%s, ticketHash=%s)",
				funcName, newFee, ticket.Hash)
		}
		w.sendJSONResponse(types.FeeAddressResponse{
			Timestamp:  now.Unix(),
			Request:    reqBytes,
			FeeAddress: ticket.FeeAddress,
			FeeAmount:  ticket.FeeAmount,
			Expiration: ticket.FeeExpiration,
		}, c)

		return
	}

	// Beyond this point we are processing a new ticket which the VSP has not
	// seen before.

	fee, err := w.getCurrentFee(dcrdClient)
	if err != nil {
		w.log.Errorf("%s: getCurrentFee error (ticketHash=%s): %v", funcName, ticketHash, err)
		w.sendError(types.ErrInternalError, c)
		return
	}

	newAddress, newAddressIdx, err := w.getNewFeeAddress()
	if err != nil {
		w.log.Errorf("%s: getNewFeeAddress error (ticketHash=%s): %v", funcName, ticketHash, err)
		w.sendError(types.ErrInternalError, c)
		return
	}

	now := time.Now()
	expire := now.Add(feeAddressExpiration).Unix()

	// Only set purchase height if the ticket already has 6 confs, otherwise its
	// purchase height may change due to reorgs.
	confirmed := false
	purchaseHeight := int64(0)
	if rawTicket.Confirmations >= w.cfg.Network.RequiredConfs {
		confirmed = true
		purchaseHeight = rawTicket.BlockHeight
	}

	dbTicket := database.Ticket{
		Hash:              ticketHash,
		PurchaseHeight:    purchaseHeight,
		CommitmentAddress: commitmentAddress,
		FeeAddressIndex:   newAddressIdx,
		FeeAddressXPubID:  w.addrGen.xPubID(),
		FeeAddress:        newAddress,
		Confirmed:         confirmed,
		FeeAmount:         int64(fee),
		FeeExpiration:     expire,
		FeeTxStatus:       database.NoFee,
	}

	err = w.db.InsertNewTicket(dbTicket)
	if err != nil {
		w.log.Errorf("%s: db.InsertNewTicket failed (ticketHash=%s): %v", funcName, ticketHash, err)
		w.sendError(types.ErrInternalError, c)
		return
	}

	w.log.Debugf("%s: Fee address created for new ticket: (tktConfirmed=%t, feeAddrIdx=%d, "+
		"feeAddr=%s, feeAmt=%s, ticketHash=%s)",
		funcName, confirmed, newAddressIdx, newAddress, fee, ticketHash)

	w.sendJSONResponse(types.FeeAddressResponse{
		Timestamp:  now.Unix(),
		Request:    reqBytes,
		FeeAddress: newAddress,
		FeeAmount:  int64(fee),
		Expiration: expire,
	}, c)
}
