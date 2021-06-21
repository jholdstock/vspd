// Copyright (c) 2020 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package rpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/decred/dcrd/chaincfg/v3"
	dcrdtypes "github.com/decred/dcrd/rpc/jsonrpc/types/v3"
	"github.com/decred/dcrd/wire"
	"github.com/jrick/bitset"
	"github.com/jrick/wsrpc/v2"
)

var (
	requiredDcrdVersion = semver{Major: 7, Minor: 0, Patch: 0}
)

// These error codes are defined in dcrd/dcrjson. They are copied here so we
// dont need to import the whole package.
const (
	ErrRPCDuplicateTx = -40
	ErrNoTxInfo       = -5
)

// DcrdRPC provides methods for calling dcrd JSON-RPCs without exposing the details
// of JSON encoding.
type DcrdRPC struct {
	Caller
	ctx context.Context
}

type DcrdConnect struct {
	*client
}

func SetupDcrd(user, pass, addr string, cert []byte, n wsrpc.Notifier) DcrdConnect {
	return DcrdConnect{setup(user, pass, addr, cert, n)}
}

// Client creates a new DcrdRPC client instance. Returns an error if dialing
// dcrd fails or if dcrd is misconfigured.
func (d *DcrdConnect) Client(ctx context.Context, netParams *chaincfg.Params) (*DcrdRPC, error) {
	c, newConnection, err := d.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dcrd connection error: %w", err)
	}

	// If this is a reused connection, we don't need to validate the dcrd config
	// again.
	if !newConnection {
		return &DcrdRPC{c, ctx}, nil
	}

	// Verify dcrd is at the required api version.
	var verMap map[string]dcrdtypes.VersionResult
	err = c.Call(ctx, "version", &verMap)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("dcrd version check failed: %w", err)
	}

	ver, exists := verMap["dcrdjsonrpcapi"]
	if !exists {
		d.Close()
		return nil, fmt.Errorf("dcrd version response missing 'dcrdjsonrpcapi'")
	}

	sVer := semver{ver.Major, ver.Minor, ver.Patch}
	if !semverCompatible(requiredDcrdVersion, sVer) {
		d.Close()
		return nil, fmt.Errorf("dcrd has incompatible JSON-RPC version: got %s, expected %s",
			sVer, requiredDcrdVersion)
	}

	// Verify dcrd is on the correct network.
	var netID wire.CurrencyNet
	err = c.Call(ctx, "getcurrentnet", &netID)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("dcrd getcurrentnet check failed: %w", err)
	}
	if netID != netParams.Net {
		d.Close()
		return nil, fmt.Errorf("dcrd running on %s, expected %s", netID, netParams.Net)
	}

	// Verify dcrd has tx index enabled (required for getrawtransaction).
	var info dcrdtypes.InfoChainResult
	err = c.Call(ctx, "getinfo", &info)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("dcrd getinfo check failed: %w", err)
	}
	if !info.TxIndex {
		d.Close()
		return nil, errors.New("dcrd does not have transaction index enabled (--txindex)")
	}

	return &DcrdRPC{c, ctx}, nil
}

// GetRawTransaction uses getrawtransaction RPC to retrieve details about the
// transaction with the provided hash.
func (c *DcrdRPC) GetRawTransaction(txHash string) (*dcrdtypes.TxRawResult, error) {
	verbose := 1
	var resp dcrdtypes.TxRawResult
	err := c.Call(c.ctx, "getrawtransaction", &resp, txHash, verbose)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// SendRawTransaction uses sendrawtransaction RPC to broadcast a transaction to
// the network. It ignores errors caused by duplicate transactions.
func (c *DcrdRPC) SendRawTransaction(txHex string) error {
	allowHighFees := false
	err := c.Call(c.ctx, "sendrawtransaction", nil, txHex, allowHighFees)
	if err != nil {

		// sendrawtransaction can return two different errors when a tx already
		// exists:
		//
		// If the tx is in mempool...
		//  Error code: -40
		//  message: rejected transaction <hash>: already have transaction <hash>
		//
		// If the tx is in a mined block...
		//  Error code: -1
		//  message: rejected transaction <hash>: transaction already exists
		//
		// It's not a problem if the transaction has already been broadcast, so
		// we will capture these errors and return nil.

		// Exists in mempool.
		var e *wsrpc.Error
		if errors.As(err, &e) && e.Code == ErrRPCDuplicateTx {
			return nil
		}

		// Exists in mined block.
		// We cannot use error code -1 here because it is a generic code for
		// many errors, so we instead need to string match on the message.
		if strings.Contains(err.Error(), "transaction already exists") {
			return nil
		}

		return err
	}
	return nil
}

// NotifyBlocks uses notifyblocks RPC to request new block notifications from dcrd.
func (c *DcrdRPC) NotifyBlocks() error {
	return c.Call(c.ctx, "notifyblocks", nil)
}

// GetBestBlockHeader uses getbestblockhash RPC, followed by getblockheader RPC,
// to retrieve the header of the best block known to the dcrd instance.
func (c *DcrdRPC) GetBestBlockHeader() (*dcrdtypes.GetBlockHeaderVerboseResult, error) {
	var bestBlockHash string
	err := c.Call(c.ctx, "getbestblockhash", &bestBlockHash)
	if err != nil {
		return nil, err
	}

	verbose := true
	var blockHeader dcrdtypes.GetBlockHeaderVerboseResult
	err = c.Call(c.ctx, "getblockheader", &blockHeader, bestBlockHash, verbose)
	if err != nil {
		return nil, err
	}
	return &blockHeader, nil
}

// ExistsLiveTicket uses existslivetickets RPC to check if the provided ticket
// hash is a live ticket known to the dcrd instance.
func (c *DcrdRPC) ExistsLiveTicket(ticketHash string) (bool, error) {
	var exists string
	err := c.Call(c.ctx, "existslivetickets", &exists, []string{ticketHash})
	if err != nil {
		return false, err
	}

	existsBytes := make([]byte, hex.DecodedLen(len(exists)))
	_, err = hex.Decode(existsBytes, []byte(exists))
	if err != nil {
		return false, err
	}

	return bitset.Bytes(existsBytes).Get(0), nil
}

// CanTicketVote checks determines whether a ticket is able to vote at some
// point in the future by checking that it is currently either immature or live.
func (c *DcrdRPC) CanTicketVote(rawTx *dcrdtypes.TxRawResult, ticketHash string, netParams *chaincfg.Params) (bool, error) {

	// Tickets which have more than (TicketMaturity+TicketExpiry+1)
	// confirmations are too old to vote.
	if rawTx.Confirmations > int64(uint32(netParams.TicketMaturity)+netParams.TicketExpiry)+1 {
		return false, nil
	}

	// If ticket is currently immature, it will be able to vote in future.
	if rawTx.Confirmations <= int64(netParams.TicketMaturity) {
		return true, nil
	}

	// If ticket is currently live, it will be able to vote in future.
	live, err := c.ExistsLiveTicket(ticketHash)
	if err != nil {
		return false, err
	}

	return live, nil
}

// ParseBlockConnectedNotification extracts the block header from a
// blockconnected JSON-RPC notification.
func ParseBlockConnectedNotification(params json.RawMessage) (*wire.BlockHeader, error) {
	var notif []string
	err := json.Unmarshal(params, &notif)
	if err != nil {
		return nil, fmt.Errorf("json unmarshal error: %w", err)
	}

	if len(notif) == 0 {
		return nil, errors.New("notification is empty")
	}

	rawHeader := notif[0]
	var header wire.BlockHeader
	err = header.Deserialize(hex.NewDecoder(bytes.NewReader([]byte(rawHeader))))
	if err != nil {
		return nil, fmt.Errorf("error creating block header from bytes: %w", err)
	}

	return &header, nil
}
