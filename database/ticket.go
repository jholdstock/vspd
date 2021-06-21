// Copyright (c) 2020-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package database

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// FeeStatus represents the current state of a ticket fee payment.
type FeeStatus string

const (
	// NoFee indicates no fee tx has been received yet.
	NoFee FeeStatus = "none"
	// FeeReceieved indicates fee tx has been received but not broadcast.
	FeeReceieved FeeStatus = "received"
	// FeeBroadcast indicates fee tx has been broadcast but not confirmed.
	FeeBroadcast FeeStatus = "broadcast"
	// FeeConfirmed indicates fee tx has been broadcast and confirmed.
	FeeConfirmed FeeStatus = "confirmed"
	// FeeError indicates fee tx could not be broadcast due to an error.
	FeeError FeeStatus = "error"
)

// TicketOutcome describes the reason a ticket is no longer votable.
type TicketOutcome string

const (
	// Revoked indicates the ticket has been revoked, either because it was
	// missed or it expired.
	Revoked TicketOutcome = "revoked"
	// Voted indicates the ticket has already voted.
	Voted TicketOutcome = "voted"
)

// The keys used to store ticket values in the database.
var (
	hashK              = []byte("Hash")
	purchaseHeightK    = []byte("PurchaseHeight")
	commitmentAddressK = []byte("CommitmentAddress")
	feeAddressIndexK   = []byte("FeeAddressIndex")
	feeAddressK        = []byte("FeeAddress")
	feeAmountK         = []byte("FeeAmount")
	feeExpirationK     = []byte("FeeExpiration")
	confirmedK         = []byte("Confirmed")
	votingWIFK         = []byte("VotingWIF")
	voteChoicesK       = []byte("VoteChoices")
	tSpendPolicyK      = []byte("TSpendPolicy")
	treasuryPolicyK    = []byte("TreasuryPolicy")
	feeTxHexK          = []byte("FeeTxHex")
	feeTxHashK         = []byte("FeeTxHash")
	feeTxStatusK       = []byte("FeeTxStatus")
	outcomeK           = []byte("Outcome")
)

type Ticket struct {
	Hash              string
	PurchaseHeight    int64
	CommitmentAddress string
	FeeAddressIndex   uint32
	FeeAddress        string
	FeeAmount         int64
	FeeExpiration     int64

	// Confirmed will be set when the ticket has 6+ confirmations.
	Confirmed bool

	// VotingWIF is set in /payfee.
	VotingWIF string

	// VoteChoices, TSpendPolicy and TreasuryPolicy are initially set in
	// /payfee, but can be updated in /setvotechoices.
	VoteChoices    map[string]string
	TSpendPolicy   map[string]string
	TreasuryPolicy map[string]string

	// FeeTxHex and FeeTxHash will be set when the fee tx has been received.
	FeeTxHex  string
	FeeTxHash string

	// FeeTxStatus indicates the current state of the fee transaction.
	FeeTxStatus FeeStatus

	// Outcome is set once a ticket is either voted or revoked. An empty outcome
	// indicates that a ticket is still votable.
	Outcome TicketOutcome
}

func (t *Ticket) FeeExpired() bool {
	now := time.Now()
	return now.After(time.Unix(t.FeeExpiration, 0))
}

// InsertNewTicket will insert the provided ticket into the database. Returns an
// error if either the ticket hash or fee address already exist.
func (vdb *VspDatabase) InsertNewTicket(ticket Ticket) error {
	vdb.ticketsMtx.Lock()
	defer vdb.ticketsMtx.Unlock()

	return vdb.db.Update(func(tx *bolt.Tx) error {
		ticketBkt := tx.Bucket(vspBktK).Bucket(ticketBktK)

		// Create a bucket for the new ticket. Returns an error if bucket
		// already exists.
		newTicketBkt, err := ticketBkt.CreateBucket([]byte(ticket.Hash))
		if err != nil {
			return fmt.Errorf("could not create bucket for ticket: %w", err)
		}

		// Error if a ticket already exists with the same fee address.
		err = ticketBkt.ForEach(func(k, v []byte) error {
			tbkt := ticketBkt.Bucket(k)

			if string(tbkt.Get(feeAddressK)) == ticket.FeeAddress {
				return fmt.Errorf("ticket with fee address %s already exists", ticket.FeeAddress)
			}

			return nil
		})
		if err != nil {
			return err
		}

		err = putTicketInBucket(newTicketBkt, ticket)
		if err != nil {
			return fmt.Errorf("putting ticket in bucket failed: %w", err)
		}

		return nil
	})
}

// putTicketInBucket encodes each of the fields of the provided ticket as a byte
// array, and stores them as values within the provided db bucket.
func putTicketInBucket(bkt *bolt.Bucket, ticket Ticket) error {
	var err error
	if err = bkt.Put(hashK, []byte(ticket.Hash)); err != nil {
		return err
	}
	if err = bkt.Put(commitmentAddressK, []byte(ticket.CommitmentAddress)); err != nil {
		return err
	}
	if err = bkt.Put(feeAddressK, []byte(ticket.FeeAddress)); err != nil {
		return err
	}
	if err = bkt.Put(votingWIFK, []byte(ticket.VotingWIF)); err != nil {
		return err
	}
	if err = bkt.Put(feeTxHexK, []byte(ticket.FeeTxHex)); err != nil {
		return err
	}
	if err = bkt.Put(feeTxHashK, []byte(ticket.FeeTxHash)); err != nil {
		return err
	}
	if err = bkt.Put(feeTxStatusK, []byte(ticket.FeeTxStatus)); err != nil {
		return err
	}
	if err = bkt.Put(outcomeK, []byte(ticket.Outcome)); err != nil {
		return err
	}
	if err = bkt.Put(purchaseHeightK, int64ToBytes(ticket.PurchaseHeight)); err != nil {
		return err
	}
	if err = bkt.Put(feeAddressIndexK, uint32ToBytes(ticket.FeeAddressIndex)); err != nil {
		return err
	}
	if err = bkt.Put(feeAmountK, int64ToBytes(ticket.FeeAmount)); err != nil {
		return err
	}
	if err = bkt.Put(feeExpirationK, int64ToBytes(ticket.FeeExpiration)); err != nil {
		return err
	}
	if err = bkt.Put(confirmedK, boolToBytes(ticket.Confirmed)); err != nil {
		return err
	}

	jsonTSpend, err := json.Marshal(ticket.TSpendPolicy)
	if err != nil {
		return err
	}
	if err = bkt.Put(tSpendPolicyK, jsonTSpend); err != nil {
		return err
	}

	jsonTreasury, err := json.Marshal(ticket.TreasuryPolicy)
	if err != nil {
		return err
	}
	if err = bkt.Put(treasuryPolicyK, jsonTreasury); err != nil {
		return err
	}

	jsonVoteChoices, err := json.Marshal(ticket.VoteChoices)
	if err != nil {
		return err
	}
	return bkt.Put(voteChoicesK, jsonVoteChoices)
}

func getTicketFromBkt(bkt *bolt.Bucket) (Ticket, error) {
	var ticket Ticket

	ticket.Hash = string(bkt.Get(hashK))
	ticket.CommitmentAddress = string(bkt.Get(commitmentAddressK))
	ticket.FeeAddress = string(bkt.Get(feeAddressK))
	ticket.VotingWIF = string(bkt.Get(votingWIFK))
	ticket.FeeTxHex = string(bkt.Get(feeTxHexK))
	ticket.FeeTxHash = string(bkt.Get(feeTxHashK))
	ticket.FeeTxStatus = FeeStatus(bkt.Get(feeTxStatusK))
	ticket.Outcome = TicketOutcome(bkt.Get(outcomeK))

	ticket.PurchaseHeight = bytesToInt64(bkt.Get(purchaseHeightK))
	ticket.FeeAddressIndex = bytesToUint32(bkt.Get(feeAddressIndexK))
	ticket.FeeAmount = bytesToInt64(bkt.Get(feeAmountK))
	ticket.FeeExpiration = bytesToInt64(bkt.Get(feeExpirationK))

	ticket.Confirmed = bytesToBool(bkt.Get(confirmedK))

	ticket.VoteChoices = make(map[string]string)
	err := json.Unmarshal(bkt.Get(voteChoicesK), &ticket.VoteChoices)
	if err != nil {
		return ticket, err
	}

	ticket.TSpendPolicy = make(map[string]string)
	err = json.Unmarshal(bkt.Get(tSpendPolicyK), &ticket.TSpendPolicy)
	if err != nil {
		return ticket, err
	}

	ticket.TreasuryPolicy = make(map[string]string)
	err = json.Unmarshal(bkt.Get(treasuryPolicyK), &ticket.TreasuryPolicy)
	if err != nil {
		return ticket, err
	}

	return ticket, nil
}

func (vdb *VspDatabase) DeleteTicket(ticket Ticket) error {
	vdb.ticketsMtx.Lock()
	defer vdb.ticketsMtx.Unlock()

	return vdb.db.Update(func(tx *bolt.Tx) error {
		ticketBkt := tx.Bucket(vspBktK).Bucket(ticketBktK)

		err := ticketBkt.DeleteBucket([]byte(ticket.Hash))
		if err != nil {
			return fmt.Errorf("could not delete ticket: %w", err)
		}

		return nil
	})
}

func (vdb *VspDatabase) UpdateTicket(ticket Ticket) error {
	vdb.ticketsMtx.Lock()
	defer vdb.ticketsMtx.Unlock()

	return vdb.db.Update(func(tx *bolt.Tx) error {
		ticketBkt := tx.Bucket(vspBktK).Bucket(ticketBktK)

		bkt := ticketBkt.Bucket([]byte(ticket.Hash))

		if bkt == nil {
			return fmt.Errorf("ticket does not exist with hash %s", ticket.Hash)
		}

		return putTicketInBucket(bkt, ticket)
	})
}

func (vdb *VspDatabase) GetTicketByHash(ticketHash string) (Ticket, bool, error) {
	vdb.ticketsMtx.RLock()
	defer vdb.ticketsMtx.RUnlock()

	var ticket Ticket
	var found bool
	err := vdb.db.View(func(tx *bolt.Tx) error {
		ticketBkt := tx.Bucket(vspBktK).Bucket(ticketBktK).Bucket([]byte(ticketHash))

		if ticketBkt == nil {
			return nil
		}

		var err error
		ticket, err = getTicketFromBkt(ticketBkt)
		if err != nil {
			return fmt.Errorf("could not get ticket: %w", err)
		}

		found = true

		return nil
	})

	return ticket, found, err
}

// Size returns the current size of the database in bytes. Note that this may
// not exactly match the size of the database file stored on disk.
func (vdb *VspDatabase) Size() (uint64, error) {
	var size uint64
	err := vdb.db.View(func(tx *bolt.Tx) error {
		size = uint64(tx.Size())
		return nil
	})
	return size, err
}

// CountTickets returns the total number of voted, revoked, and currently voting
// tickets. This func iterates over every ticket so should be used sparingly.
func (vdb *VspDatabase) CountTickets() (int64, int64, int64, error) {
	vdb.ticketsMtx.RLock()
	defer vdb.ticketsMtx.RUnlock()

	var voting, voted, revoked int64
	err := vdb.db.View(func(tx *bolt.Tx) error {
		ticketBkt := tx.Bucket(vspBktK).Bucket(ticketBktK)

		return ticketBkt.ForEach(func(k, v []byte) error {
			tBkt := ticketBkt.Bucket(k)

			if FeeStatus(tBkt.Get(feeTxStatusK)) == FeeConfirmed {
				switch TicketOutcome(tBkt.Get(outcomeK)) {
				case Voted:
					voted++
				case Revoked:
					revoked++
				default:
					voting++
				}
			}

			return nil
		})
	})

	return voting, voted, revoked, err
}

// GetUnconfirmedTickets returns tickets which are not yet confirmed.
func (vdb *VspDatabase) GetUnconfirmedTickets() ([]Ticket, error) {
	vdb.ticketsMtx.RLock()
	defer vdb.ticketsMtx.RUnlock()

	return vdb.filterTickets(func(t *bolt.Bucket) bool {
		return !bytesToBool(t.Get(confirmedK))
	})
}

// GetPendingFees returns tickets which are confirmed and have a fee tx which is
// not yet broadcast.
func (vdb *VspDatabase) GetPendingFees() ([]Ticket, error) {
	vdb.ticketsMtx.RLock()
	defer vdb.ticketsMtx.RUnlock()

	return vdb.filterTickets(func(t *bolt.Bucket) bool {
		return bytesToBool(t.Get(confirmedK)) && FeeStatus(t.Get(feeTxStatusK)) == FeeReceieved
	})
}

// GetUnconfirmedFees returns tickets with a fee tx that is broadcast but not
// confirmed yet.
func (vdb *VspDatabase) GetUnconfirmedFees() ([]Ticket, error) {
	vdb.ticketsMtx.RLock()
	defer vdb.ticketsMtx.RUnlock()

	return vdb.filterTickets(func(t *bolt.Bucket) bool {
		return FeeStatus(t.Get(feeTxStatusK)) == FeeBroadcast
	})
}

// GetVotableTickets returns tickets with a confirmed fee tx and no outcome (ie.
// not expired/voted/missed).
func (vdb *VspDatabase) GetVotableTickets() ([]Ticket, error) {
	return vdb.filterTickets(func(t *bolt.Bucket) bool {
		return FeeStatus(t.Get(feeTxStatusK)) == FeeConfirmed && TicketOutcome(t.Get(outcomeK)) == ""
	})
}

// filterTickets accepts a filter function and returns all tickets from the
// database which match the filter.
//
// This function must be called with the lock held.
func (vdb *VspDatabase) filterTickets(filter func(*bolt.Bucket) bool) ([]Ticket, error) {
	var tickets []Ticket
	err := vdb.db.View(func(tx *bolt.Tx) error {
		ticketBkt := tx.Bucket(vspBktK).Bucket(ticketBktK)

		return ticketBkt.ForEach(func(k, v []byte) error {
			ticketBkt := ticketBkt.Bucket(k)

			if filter(ticketBkt) {
				ticket, err := getTicketFromBkt(ticketBkt)
				if err != nil {
					return fmt.Errorf("could not get ticket: %w", err)
				}
				tickets = append(tickets, ticket)
			}

			return nil
		})
	})

	return tickets, err
}
