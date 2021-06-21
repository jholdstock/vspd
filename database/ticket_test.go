// Copyright (c) 2020-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package database

import (
	"math/rand"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

var seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))

// randString randomly generates a string of the requested length, using only
// characters from the provided charset.
func randString(length int, charset string) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(b)
}

func exampleTicket() Ticket {
	const hexCharset = "1234567890abcdef"
	const addrCharset = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

	return Ticket{
		Hash:              randString(64, hexCharset),
		CommitmentAddress: randString(35, addrCharset),
		FeeAddressIndex:   12345,
		FeeAddress:        randString(35, addrCharset),
		FeeAmount:         10000000,
		FeeExpiration:     4,
		Confirmed:         false,
		VoteChoices:       map[string]string{"AgendaID": "yes"},
		TSpendPolicy:      map[string]string{randString(33, hexCharset): "no"},
		TreasuryPolicy:    map[string]string{randString(64, hexCharset): "abstain"},
		VotingWIF:         randString(53, addrCharset),
		FeeTxHex:          randString(504, hexCharset),
		FeeTxHash:         randString(64, hexCharset),
		FeeTxStatus:       FeeBroadcast,
	}
}

func testInsertNewTicket(t *testing.T) {
	// Insert a ticket into the database.
	ticket := exampleTicket()
	err := db.InsertNewTicket(ticket)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	// Insert another ticket.
	err = db.InsertNewTicket(exampleTicket())
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	// Inserting a ticket with the same hash should fail.
	ticket2 := exampleTicket()
	ticket2.Hash = ticket.Hash
	err = db.InsertNewTicket(ticket2)
	if err == nil {
		t.Fatal("expected an error inserting ticket with duplicate hash")
	}

	// Inserting a ticket with the same fee address should fail.
	ticket3 := exampleTicket()
	ticket3.FeeAddress = ticket.FeeAddress
	err = db.InsertNewTicket(ticket3)
	if err == nil {
		t.Fatal("expected an error inserting ticket with duplicate fee addr")
	}

	// Inserting a ticket with empty hash should fail.
	ticket.Hash = ""
	err = db.InsertNewTicket(ticket)
	if err == nil {
		t.Fatal("expected an error inserting ticket with no hash")
	}
}

func testDeleteTicket(t *testing.T) {
	// Insert a ticket into the database.
	ticket := exampleTicket()
	err := db.InsertNewTicket(ticket)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	// Delete ticket.
	err = db.DeleteTicket(ticket)
	if err != nil {
		t.Fatalf("error deleting ticket: %v", err)
	}

	// Nothing should be in the db.
	_, found, err := db.GetTicketByHash(ticket.Hash)
	if err != nil {
		t.Fatalf("error retrieving ticket by ticket hash: %v", err)
	}
	if found {
		t.Fatal("expected found==false")
	}
}

func testGetTicketByHash(t *testing.T) {
	// Insert a ticket into the database.
	ticket := exampleTicket()
	err := db.InsertNewTicket(ticket)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	// Retrieve ticket from database.
	retrieved, found, err := db.GetTicketByHash(ticket.Hash)
	if err != nil {
		t.Fatalf("error retrieving ticket by ticket hash: %v", err)
	}
	if !found {
		t.Fatal("expected found==true")
	}

	// Check ticket fields match expected.
	if retrieved.Hash != ticket.Hash ||
		retrieved.CommitmentAddress != ticket.CommitmentAddress ||
		retrieved.FeeAddressIndex != ticket.FeeAddressIndex ||
		retrieved.FeeAddress != ticket.FeeAddress ||
		retrieved.FeeAmount != ticket.FeeAmount ||
		retrieved.FeeExpiration != ticket.FeeExpiration ||
		retrieved.Confirmed != ticket.Confirmed ||
		!reflect.DeepEqual(retrieved.VoteChoices, ticket.VoteChoices) ||
		!reflect.DeepEqual(retrieved.TSpendPolicy, ticket.TSpendPolicy) ||
		!reflect.DeepEqual(retrieved.TreasuryPolicy, ticket.TreasuryPolicy) ||
		retrieved.VotingWIF != ticket.VotingWIF ||
		retrieved.FeeTxHex != ticket.FeeTxHex ||
		retrieved.FeeTxHash != ticket.FeeTxHash ||
		retrieved.FeeTxStatus != ticket.FeeTxStatus {
		t.Fatal("retrieved ticket value didnt match expected")
	}

	// Check found==false when requesting a non-existent ticket.
	_, found, err = db.GetTicketByHash("Not a real ticket hash")
	if err != nil {
		t.Fatalf("error retrieving ticket by ticket hash: %v", err)
	}
	if found {
		t.Fatal("expected found==false")
	}
}

func testUpdateTicket(t *testing.T) {
	ticket := exampleTicket()
	// Insert a ticket into the database.
	err := db.InsertNewTicket(ticket)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	// Update ticket with new values.
	ticket.FeeAmount = ticket.FeeAmount + 1
	ticket.FeeExpiration = ticket.FeeExpiration + 1
	ticket.VoteChoices = map[string]string{"New agenda": "New value"}

	err = db.UpdateTicket(ticket)
	if err != nil {
		t.Fatalf("error updating ticket: %v", err)
	}

	// Retrieve updated ticket from database.
	retrieved, found, err := db.GetTicketByHash(ticket.Hash)
	if err != nil {
		t.Fatalf("error retrieving ticket by ticket hash: %v", err)
	}
	if !found {
		t.Fatal("expected found==true")
	}

	if ticket.FeeAmount != retrieved.FeeAmount ||
		ticket.FeeExpiration != retrieved.FeeExpiration ||
		!reflect.DeepEqual(retrieved.VoteChoices, ticket.VoteChoices) {
		t.Fatal("retrieved ticket value didnt match expected")
	}

	// Updating a non-existent ticket should fail.
	ticket.Hash = "doesnt exist"
	err = db.UpdateTicket(ticket)
	if err == nil {
		t.Fatal("expected an error updating a ticket with non-existent hash")
	}
}

func testTicketFeeExpired(t *testing.T) {
	ticket := exampleTicket()

	now := time.Now()
	hourBefore := now.Add(-time.Hour).Unix()
	hourAfter := now.Add(time.Hour).Unix()

	ticket.FeeExpiration = hourAfter
	if ticket.FeeExpired() {
		t.Fatal("expected ticket not to be expired")
	}

	ticket.FeeExpiration = hourBefore
	if !ticket.FeeExpired() {
		t.Fatal("expected ticket to be expired")
	}
}

func testFilterTickets(t *testing.T) {
	// Insert a ticket.
	ticket := exampleTicket()
	err := db.InsertNewTicket(ticket)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	// Insert another ticket.
	ticket2 := exampleTicket()
	ticket2.Confirmed = !ticket.Confirmed
	err = db.InsertNewTicket(ticket2)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	// Expect all tickets returned.
	retrieved, err := db.filterTickets(func(t *bolt.Bucket) bool {
		return true
	})
	if err != nil {
		t.Fatalf("error filtering tickets: %v", err)
	}
	if len(retrieved) != 2 {
		t.Fatalf("expected to find 2 tickets, found %d", len(retrieved))
	}

	// Only one ticket should be confirmed.
	retrieved, err = db.filterTickets(func(t *bolt.Bucket) bool {
		return bytesToBool(t.Get(confirmedK))
	})
	if err != nil {
		t.Fatalf("error filtering tickets: %v", err)
	}
	if len(retrieved) != 1 {
		t.Fatalf("expected to find 1 ticket, found %d", len(retrieved))
	}
	if retrieved[0].Confirmed != true {
		t.Fatal("expected retrieved ticket to be confirmed")
	}

	// Expect no tickets with confirmed fee.
	retrieved, err = db.filterTickets(func(t *bolt.Bucket) bool {
		return FeeStatus(t.Get(feeTxStatusK)) == FeeConfirmed
	})
	if err != nil {
		t.Fatalf("error filtering tickets: %v", err)
	}
	if len(retrieved) != 0 {
		t.Fatalf("expected to find 0 tickets, found %d", len(retrieved))
	}
}

func testCountTickets(t *testing.T) {
	count := func(test string, expectedVoting, expectedVoted, expectedRevoked int64) {
		voting, voted, revoked, err := db.CountTickets()
		if err != nil {
			t.Fatalf("error counting tickets: %v", err)
		}

		if voting != expectedVoting {
			t.Fatalf("test %s: expected %d voting tickets, got %d",
				test, expectedVoting, voting)
		}
		if voted != expectedVoted {
			t.Fatalf("test %s: expected %d voted tickets, got %d",
				test, expectedVoted, voted)
		}
		if revoked != expectedRevoked {
			t.Fatalf("test %s: expected %d revoked tickets, got %d",
				test, expectedRevoked, revoked)
		}
	}

	// Initial counts should all be zero.
	count("empty db", 0, 0, 0)

	// Insert a ticket with non-confirmed fee into the database.
	// This should not be counted.
	ticket := exampleTicket()
	ticket.FeeTxStatus = FeeReceieved
	err := db.InsertNewTicket(ticket)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	count("unconfirmed fee", 0, 0, 0)

	// Insert a ticket with confirmed fee into the database.
	// This should be counted.
	ticket2 := exampleTicket()
	ticket2.FeeTxStatus = FeeConfirmed
	err = db.InsertNewTicket(ticket2)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	count("confirmed fee", 1, 0, 0)

	// Insert a voted ticket into the database.
	// This should be counted.
	ticket3 := exampleTicket()
	ticket3.FeeTxStatus = FeeConfirmed
	ticket3.Outcome = Voted
	err = db.InsertNewTicket(ticket3)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	count("voted", 1, 1, 0)

	// Insert a revoked ticket into the database.
	// This should be counted.
	ticket4 := exampleTicket()
	ticket4.FeeTxStatus = FeeConfirmed
	ticket4.Outcome = Revoked
	err = db.InsertNewTicket(ticket4)
	if err != nil {
		t.Fatalf("error storing ticket in database: %v", err)
	}

	count("revoked", 1, 1, 1)
}
