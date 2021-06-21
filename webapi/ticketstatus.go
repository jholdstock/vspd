// Copyright (c) 2020 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package webapi

import (
	"time"

	"github.com/decred/vspd/database"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

// ticketStatus is the handler for "POST /api/v3/ticketstatus".
func ticketStatus(c *gin.Context) {
	const funcName = "ticketStatus"

	// Get values which have been added to context by middleware.
	ticket := c.MustGet("Ticket").(database.Ticket)
	knownTicket := c.MustGet("KnownTicket").(bool)
	reqBytes := c.MustGet("RequestBytes").([]byte)

	if !knownTicket {
		log.Warnf("%s: Unknown ticket (clientIP=%s)", funcName, c.ClientIP())
		sendError(errUnknownTicket, c)
		return
	}

	var request ticketStatusRequest
	if err := binding.JSON.BindBody(reqBytes, &request); err != nil {
		log.Warnf("%s: Bad request (clientIP=%s): %v", funcName, c.ClientIP(), err)
		sendErrorWithMsg(err.Error(), errBadRequest, c)
		return
	}

	sendJSONResponse(ticketStatusResponse{
		Timestamp:       time.Now().Unix(),
		Request:         reqBytes,
		TicketConfirmed: ticket.Confirmed,
		FeeTxStatus:     string(ticket.FeeTxStatus),
		FeeTxHash:       ticket.FeeTxHash,
		VoteChoices:     ticket.VoteChoices,
		TreasuryPolicy:  ticket.TreasuryPolicy,
		TSpendPolicy:    ticket.TSpendPolicy,
	}, c)
}
