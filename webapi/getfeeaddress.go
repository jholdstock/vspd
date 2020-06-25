package webapi

import (
	"sync"
	"time"

	"decred.org/dcrwallet/wallet/txrules"
	"github.com/decred/dcrd/dcrutil/v3"
	"github.com/decred/vspd/database"
	"github.com/decred/vspd/rpc"
	"github.com/gin-gonic/gin"
)

// addrMtx protects getNewFeeAddress.
var addrMtx sync.Mutex

// getNewFeeAddress gets a new address from the address generator and stores the
// new address index in the database. In order to maintain consistency between
// the internal counter of address generator and the database, this function
// cannot be run concurrently.
func getNewFeeAddress(db *database.VspDatabase, addrGen *addressGenerator) (string, uint32, error) {
	addrMtx.Lock()
	defer addrMtx.Unlock()

	addr, idx, err := addrGen.NextAddress()
	if err != nil {
		return "", 0, err
	}

	err = db.SetLastAddressIndex(idx)
	if err != nil {
		return "", 0, err
	}

	return addr, idx, nil
}

func getCurrentFee(dcrdClient *rpc.DcrdRPC) (dcrutil.Amount, error) {
	bestBlock, err := dcrdClient.GetBestBlockHeader()
	if err != nil {
		return 0, err
	}
	sDiff, err := dcrutil.NewAmount(bestBlock.SBits)
	if err != nil {
		return 0, err
	}
	relayFee, err := dcrutil.NewAmount(relayFee)
	if err != nil {
		return 0, err
	}

	fee := txrules.StakePoolTicketFee(sDiff, relayFee, int32(bestBlock.Height),
		cfg.VSPFee, cfg.NetParams)
	if err != nil {
		return 0, err
	}
	return fee, nil
}

// feeAddress is the handler for "POST /feeaddress".
func feeAddress(c *gin.Context) {

	funcName := "feeAddress"

	// Get values which have been added to context by middleware.
	ticket := c.MustGet("Ticket").(database.Ticket)
	knownTicket := c.MustGet("KnownTicket").(bool)
	commitmentAddress := c.MustGet("CommitmentAddress").(string)
	dcrdClient := c.MustGet("DcrdClient").(*rpc.DcrdRPC)

	if cfg.VspClosed {
		sendError(errVspClosed, c)
		return
	}

	var feeAddressRequest FeeAddressRequest
	if err := c.ShouldBindJSON(&feeAddressRequest); err != nil {
		log.Warnf("%s: Bad request from %s: %v", funcName, c.ClientIP(), err)
		sendErrorWithMsg(err.Error(), errBadRequest, c)
		return
	}

	ticketHash := feeAddressRequest.TicketHash

	// Respond early if we already have the fee tx for this ticket.
	if ticket.FeeTxStatus == database.FeeReceieved ||
		ticket.FeeTxStatus == database.FeeBroadcast ||
		ticket.FeeTxStatus == database.FeeConfirmed {
		log.Warnf("%s: Fee tx already received from %s: ticketHash=%s", funcName, c.ClientIP(), ticket.Hash)
		sendError(errFeeAlreadyReceived, c)
		return
	}

	// Get ticket details.
	rawTicket, err := dcrdClient.GetRawTransaction(ticketHash)
	if err != nil {
		log.Errorf("%s: Could not retrieve tx %s for %s: %v", funcName, ticketHash, c.ClientIP(), err)
		sendError(errInternalError, c)
		return
	}

	// Ensure this ticket is eligible to vote at some point in the future.
	canVote, err := dcrdClient.CanTicketVote(rawTicket, ticketHash, cfg.NetParams)
	if err != nil {
		log.Errorf("%s: canTicketVote error: %v", funcName, err)
		sendError(errInternalError, c)
		return
	}
	if !canVote {
		log.Warnf("%s: Unvotable ticket %s from %s", funcName, ticketHash, c.ClientIP())
		sendError(errTicketCannotVote, c)
		return
	}

	// VSP already knows this ticket and has already issued it a fee address.
	if knownTicket {

		// If the expiry period has passed we need to issue a new fee.
		now := time.Now()
		if ticket.FeeExpired() {
			newFee, err := getCurrentFee(dcrdClient)
			if err != nil {
				log.Errorf("%s: getCurrentFee error: %v", funcName, err)
				sendError(errInternalError, c)
				return
			}
			ticket.FeeExpiration = now.Add(feeAddressExpiration).Unix()
			ticket.FeeAmount = int64(newFee)

			err = db.UpdateTicket(ticket)
			if err != nil {
				log.Errorf("%s: UpdateTicket error: %v", funcName, err)
				sendError(errInternalError, c)
				return
			}
			log.Debugf("%s: Expired fee updated for ticket: newFeeAmt=%f, ticketHash=%s",
				funcName, newFee, ticket.Hash)
		}
		sendJSONResponse(feeAddressResponse{
			Timestamp:  now.Unix(),
			Request:    feeAddressRequest,
			FeeAddress: ticket.FeeAddress,
			FeeAmount:  ticket.FeeAmount,
			Expiration: ticket.FeeExpiration,
		}, c)

		return
	}

	// Beyond this point we are processing a new ticket which the VSP has not
	// seen before.

	fee, err := getCurrentFee(dcrdClient)
	if err != nil {
		log.Errorf("%s: getCurrentFee error: %v", funcName, err)
		sendError(errInternalError, c)
		return
	}

	newAddress, newAddressIdx, err := getNewFeeAddress(db, addrGen)
	if err != nil {
		log.Errorf("%s: getNewFeeAddress error: %v", funcName, err)
	}

	now := time.Now()
	expire := now.Add(feeAddressExpiration).Unix()

	confirmed := rawTicket.Confirmations >= requiredConfs

	dbTicket := database.Ticket{
		Hash:              ticketHash,
		CommitmentAddress: commitmentAddress,
		FeeAddressIndex:   newAddressIdx,
		FeeAddress:        newAddress,
		Confirmed:         confirmed,
		FeeAmount:         int64(fee),
		FeeExpiration:     expire,
		FeeTxStatus:       database.NoFee,
	}

	err = db.InsertNewTicket(dbTicket)
	if err != nil {
		log.Errorf("%s: InsertTicket error: %v", funcName, err)
		sendError(errInternalError, c)
		return
	}

	log.Debugf("%s: Fee address created for new ticket: tktConfirmed=%t, feeAddrIdx=%d, "+
		"feeAddr=%s, feeAmt=%s, ticketHash=%s", funcName, confirmed, newAddressIdx, newAddress, fee, ticketHash)

	sendJSONResponse(feeAddressResponse{
		Timestamp:  now.Unix(),
		Request:    feeAddressRequest,
		FeeAddress: newAddress,
		FeeAmount:  int64(fee),
		Expiration: expire,
	}, c)
}
