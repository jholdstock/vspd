// Copyright (c) 2020-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package webapi

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/decred/dcrd/blockchain/stake/v4"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/wire"
	"github.com/gin-gonic/gin"
)

func currentVoteVersion(params *chaincfg.Params) uint32 {
	var latestVersion uint32
	for version := range params.Deployments {
		if latestVersion < version {
			latestVersion = version
		}
	}
	return latestVersion
}

// validConsensusVoteChoices returns an error if provided vote choices are not
// valid for the most recent consensus agendas.
func validConsensusVoteChoices(params *chaincfg.Params, voteVersion uint32, voteChoices map[string]string) error {

agendaLoop:
	for agenda, choice := range voteChoices {
		// Does the agenda exist?
		for _, v := range params.Deployments[voteVersion] {
			if v.Vote.Id == agenda {
				// Agenda exists - does the vote choice exist?
				for _, c := range v.Vote.Choices {
					if c.Id == choice {
						// Valid agenda and choice combo! Check the next one...
						continue agendaLoop
					}
				}
				return fmt.Errorf("choice %q not found for agenda %q", choice, agenda)
			}

		}
		return fmt.Errorf("agenda %q not found for vote version %d", agenda, voteVersion)
	}

	return nil
}

func validTreasuryPolicy(policy map[string]string) error {
	for key, choice := range policy {
		pikey, err := hex.DecodeString(key)
		if err != nil {
			return fmt.Errorf("error decoding treasury key %q: %w", key, err)
		}
		if len(pikey) != secp256k1.PubKeyBytesLenCompressed {
			return fmt.Errorf("treasury key %q is not 33 bytes", key)
		}

		err = validPolicyOption(choice)
		if err != nil {
			return err
		}
	}

	return nil
}

func validTSpendPolicy(policy map[string]string) error {
	for hash, choice := range policy {
		_, err := chainhash.NewHashFromStr(hash)
		if err != nil {
			return fmt.Errorf("error decoding tspend hash %q: %w", hash, err)
		}

		err = validPolicyOption(choice)
		if err != nil {
			return err
		}
	}

	return nil
}

func validPolicyOption(policy string) error {
	switch policy {
	case "yes", "no", "abstain", "invalid", "":
		return nil
	default:
		return fmt.Errorf("%q is not a valid policy", policy)
	}
}

func validateSignature(reqBytes []byte, commitmentAddress string, c *gin.Context) error {
	// Ensure a signature is provided.
	signature := c.GetHeader("VSP-Client-Signature")
	if signature == "" {
		return errors.New("no VSP-Client-Signature header")
	}

	err := dcrutil.VerifyMessage(commitmentAddress, signature, string(reqBytes), cfg.NetParams)
	if err != nil {
		return err
	}
	return nil
}

func decodeTransaction(txHex string) (*wire.MsgTx, error) {
	msgHex, err := hex.DecodeString(txHex)
	if err != nil {
		return nil, err
	}
	msgTx := wire.NewMsgTx()
	if err = msgTx.FromBytes(msgHex); err != nil {
		return nil, err
	}
	return msgTx, nil
}

func isValidTicket(tx *wire.MsgTx) error {
	if !stake.IsSStx(tx) {
		return errors.New("invalid transaction - not sstx")
	}
	if len(tx.TxOut) != 3 {
		return fmt.Errorf("invalid transaction - expected 3 outputs, got %d", len(tx.TxOut))
	}

	return nil
}
