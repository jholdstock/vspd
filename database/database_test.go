// Copyright (c) 2020-2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package database

import (
	"crypto/ed25519"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/decred/slog"
)

const (
	testDb               = "test.db"
	feeXPub              = "feexpub"
	maxVoteChangeRecords = 3

	// addrCharset is a list of all valid DCR address characters.
	addrCharset = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	// hexCharset is a list of all valid hexadecimal characters.
	hexCharset = "1234567890abcdef"
	// sigCharset is a list of all valid request/response signature characters
	// (base64 encoding).
	sigCharset = "0123456789ABCDEFGHJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz+/="
)

var (
	db         *VspDatabase
	seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// randBytes returns a byte slice of size n filled with random bytes.
func randBytes(n int) []byte {
	slice := make([]byte, n)
	if _, err := seededRand.Read(slice); err != nil {
		panic(err)
	}
	return slice
}

// randString randomly generates a string of the requested length, using only
// characters from the provided charset.
func randString(length int, charset string) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(b)
}

func stdoutLogger() slog.Logger {
	backend := slog.NewBackend(os.Stdout)
	log := backend.Logger("test")
	log.SetLevel(slog.LevelTrace)
	return log
}

// TestDatabase runs all database tests.
func TestDatabase(t *testing.T) {
	// Ensure we are starting with a clean environment.
	os.Remove(testDb)

	// All sub-tests to run.
	tests := map[string]func(*testing.T){
		"testCreateNew":         testCreateNew,
		"testInsertNewTicket":   testInsertNewTicket,
		"testGetTicketByHash":   testGetTicketByHash,
		"testUpdateTicket":      testUpdateTicket,
		"testTicketFeeExpired":  testTicketFeeExpired,
		"testFilterTickets":     testFilterTickets,
		"testCountTickets":      testCountTickets,
		"testAddressIndex":      testAddressIndex,
		"testDeleteTicket":      testDeleteTicket,
		"testVoteChangeRecords": testVoteChangeRecords,
		"testHTTPBackup":        testHTTPBackup,
		"testAltSignAddrData":   testAltSignAddrData,
		"testInsertAltSignAddr": testInsertAltSignAddr,
		"testDeleteAltSignAddr": testDeleteAltSignAddr,
	}

	log := stdoutLogger()

	for testName, test := range tests {

		// Create a new blank database for each sub-test.
		err := CreateNew(testDb, feeXPub)
		if err != nil {
			t.Fatalf("error creating test database: %v", err)
		}

		// Open the newly created database so it is ready to use.
		db, err = Open(testDb, log, maxVoteChangeRecords)
		if err != nil {
			t.Fatalf("error opening test database: %v", err)
		}

		// Run the sub-test.
		t.Run(testName, test)

		writeBackup := false
		db.Close(writeBackup)
		os.Remove(testDb)
	}
}

func testCreateNew(t *testing.T) {
	// A newly created DB should contain a signing keypair.
	priv, pub, err := db.KeyPair()
	if err != nil {
		t.Fatalf("error getting keypair: %v", err)
	}

	// Ensure keypair can be used for signing/verifying messages.
	msg := []byte("msg")
	sig := ed25519.Sign(priv, msg)
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatalf("keypair from database could not be used to sign/verify a message")
	}

	// A newly created DB should have a cookie secret.
	secret, err := db.CookieSecret()
	if err != nil {
		t.Fatalf("error getting cookie secret: %v", err)
	}

	if len(secret) != 32 {
		t.Fatalf("expected a 32 byte cookie secret, got %d bytes", len(secret))
	}

	// A newly created DB should store the fee xpub it was initialized with.
	retrievedXPub, err := db.FeeXPub()
	if err != nil {
		t.Fatalf("error getting fee xpub: %v", err)
	}

	if retrievedXPub != feeXPub {
		t.Fatalf("expected fee xpub %v, got %v", feeXPub, retrievedXPub)
	}
}

func testHTTPBackup(t *testing.T) {
	// Capture the HTTP response written by the backup func.
	rr := httptest.NewRecorder()
	err := db.BackupDB(rr)
	if err != nil {
		t.Fatal(err)
	}

	// Check the HTTP status is OK.
	if status := rr.Code; status != http.StatusOK {
		t.Fatalf("wrong HTTP status code: expected %v, got %v",
			http.StatusOK, status)
	}

	// Check HTTP headers.
	header := "Content-Type"
	expected := "application/octet-stream"
	if actual := rr.Header().Get(header); actual != expected {
		t.Errorf("wrong %s header: expected %s, got %s",
			header, expected, actual)
	}

	header = "Content-Disposition"
	expected = `attachment; filename="vspd.db"`
	if actual := rr.Header().Get(header); actual != expected {
		t.Errorf("wrong %s header: expected %s, got %s",
			header, expected, actual)
	}

	header = "Content-Length"
	cLength, err := strconv.Atoi(rr.Header().Get(header))
	if err != nil {
		t.Fatalf("could not convert %s to integer: %v", header, err)
	}

	if cLength <= 0 {
		t.Fatalf("expected a %s greater than zero, got %d", header, cLength)
	}

	// Check reported length matches actual.
	body, err := io.ReadAll(rr.Result().Body)
	if err != nil {
		t.Fatalf("could not read http response body: %v", err)
	}

	if len(body) != cLength {
		t.Fatalf("expected reported content-length to match actual body length. %v != %v",
			cLength, len(body))
	}
}
