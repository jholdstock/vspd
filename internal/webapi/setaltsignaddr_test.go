// Copyright (c) 2021-2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package webapi

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	dcrdtypes "github.com/decred/dcrd/rpc/jsonrpc/types/v4"
	"github.com/decred/slog"
	"github.com/decred/vspd/database"
	"github.com/decred/vspd/internal/config"
	"github.com/decred/vspd/types/v2"
	"github.com/gin-gonic/gin"
)

const (
	// hexCharset is a list of all valid hexadecimal characters.
	hexCharset = "1234567890abcdef"
	// sigCharset is a list of all valid request/response signature characters
	// (base64 encoding).
	sigCharset = "0123456789ABCDEFGHJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz+/="
	testDb     = "test.db"
)

var (
	seededRand           = rand.New(rand.NewSource(time.Now().UnixNano()))
	feeXPub              = "feexpub"
	maxVoteChangeRecords = 3
	api                  *WebAPI
)

// randBytes returns a byte slice of size n filled with random bytes.
func randBytes(n int) []byte {
	slice := make([]byte, n)
	if _, err := seededRand.Read(slice); err != nil {
		panic(err)
	}
	return slice
}

func stdoutLogger() slog.Logger {
	backend := slog.NewBackend(os.Stdout)
	log := backend.Logger("test")
	log.SetLevel(slog.LevelTrace)
	return log
}

func TestMain(m *testing.M) {

	log := stdoutLogger()

	// Set up some global params.
	cfg := Config{
		Network: &config.MainNet,
	}
	_, signPrivKey, _ := ed25519.GenerateKey(seededRand)

	// Create a database to use.
	// Ensure we are starting with a clean environment.
	os.Remove(testDb)

	// Create a new blank database for all tests.
	err := database.CreateNew(testDb, feeXPub)
	if err != nil {
		panic(fmt.Errorf("error creating test database: %w", err))
	}

	// Open the newly created database so it is ready to use.
	db, err := database.Open(testDb, log, maxVoteChangeRecords)
	if err != nil {
		panic(fmt.Errorf("error opening test database: %w", err))
	}

	api = &WebAPI{
		cfg:         cfg,
		signPrivKey: signPrivKey,
		db:          db,
		log:         log,
	}

	// Run tests.
	exitCode := m.Run()

	writeBackup := false
	db.Close(writeBackup)
	os.Remove(testDb)

	os.Exit(exitCode)
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

// Ensure that testNode satisfies Node.
var _ node = (*testNode)(nil)

type testNode struct {
	getRawTransaction    *dcrdtypes.TxRawResult
	getRawTransactionErr error
	existsLiveTicket     bool
	existsLiveTicketErr  error
}

func (n *testNode) ExistsLiveTicket(_ string) (bool, error) {
	return n.existsLiveTicket, n.existsLiveTicketErr
}

func (n *testNode) GetRawTransaction(_ string) (*dcrdtypes.TxRawResult, error) {
	return n.getRawTransaction, n.getRawTransactionErr
}

func TestSetAltSignAddress(t *testing.T) {
	const testAddr = "DsVoDXNQqyF3V83PJJ5zMdnB4pQuJHBAh15"
	tests := map[string]struct {
		dcrdClientErr         bool
		deformReq             int
		addr                  string
		node                  *testNode
		isExistingAltSignAddr bool
		wantHTTPStatus        int
		// wantErrCode and wantErrMsg only checked if wantHTTPStatus != 200.
		wantErrCode types.ErrorCode
		wantErrMsg  string
	}{
		"ok": {
			addr: testAddr,
			node: &testNode{
				getRawTransaction: &dcrdtypes.TxRawResult{
					Confirmations: 1000,
				},
				getRawTransactionErr: nil,
				existsLiveTicket:     true,
			},
			wantHTTPStatus: http.StatusOK,
		},
		"dcrd client error": {
			dcrdClientErr:  true,
			wantHTTPStatus: http.StatusInternalServerError,
			wantErrCode:    types.ErrInternalError,
			wantErrMsg:     types.ErrInternalError.DefaultMessage(),
		},
		"bad request": {
			deformReq:      1,
			wantHTTPStatus: http.StatusBadRequest,
			wantErrCode:    types.ErrBadRequest,
			wantErrMsg:     "json: cannot unmarshal string into Go value of type types.SetAltSignAddrRequest",
		},
		"bad addr": {
			addr:           "xxx",
			wantHTTPStatus: http.StatusBadRequest,
			wantErrCode:    types.ErrBadRequest,
			wantErrMsg:     "failed to decode address \"xxx\": invalid format: version and/or checksum bytes missing",
		},
		"addr wrong type": {
			addr:           "DkM3ZigNyiwHrsXRjkDQ8t8tW6uKGW9g61qEkG3bMqQPQWYEf5X3J",
			wantHTTPStatus: http.StatusBadRequest,
			wantErrCode:    types.ErrBadRequest,
			wantErrMsg:     "wrong type for alternate signing address",
		},
		"getRawTransaction error from dcrd client": {
			addr: testAddr,
			node: &testNode{
				getRawTransactionErr: errors.New("getRawTransaction error"),
			},
			wantHTTPStatus: http.StatusInternalServerError,
			wantErrCode:    types.ErrInternalError,
			wantErrMsg:     types.ErrInternalError.DefaultMessage(),
		},
		"existsLiveTicket error from dcrd client": {
			addr: testAddr,
			node: &testNode{
				getRawTransaction: &dcrdtypes.TxRawResult{
					Confirmations: 1000,
				},
				existsLiveTicketErr: errors.New("existsLiveTicket error"),
			},
			wantHTTPStatus: http.StatusInternalServerError,
			wantErrCode:    types.ErrInternalError,
			wantErrMsg:     types.ErrInternalError.DefaultMessage(),
		},
		"ticket can't vote": {
			addr: testAddr,
			node: &testNode{
				getRawTransaction: &dcrdtypes.TxRawResult{
					Confirmations: 1000,
				},
				existsLiveTicket: false,
			},
			wantHTTPStatus: http.StatusBadRequest,
			wantErrCode:    types.ErrTicketCannotVote,
			wantErrMsg:     types.ErrTicketCannotVote.DefaultMessage(),
		},
		"only one alt sign addr allowed": {
			addr: testAddr,
			node: &testNode{
				getRawTransaction: &dcrdtypes.TxRawResult{},
				existsLiveTicket:  true,
			},
			isExistingAltSignAddr: true,
			wantHTTPStatus:        http.StatusBadRequest,
			wantErrCode:           types.ErrBadRequest,
			wantErrMsg:            "alternate sign address data already exists",
		},
	}

	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			ticketHash := randString(64, hexCharset)
			req := &types.SetAltSignAddrRequest{
				Timestamp:      time.Now().Unix(),
				TicketHash:     ticketHash,
				TicketHex:      randString(504, hexCharset),
				ParentHex:      randString(504, hexCharset),
				AltSignAddress: test.addr,
			}
			reqSig := randString(504, hexCharset)
			b, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}

			if test.isExistingAltSignAddr {
				data := &database.AltSignAddrData{
					AltSignAddr: test.addr,
					Req:         string(b),
					ReqSig:      reqSig,
					Resp:        string(randBytes(1000)),
					RespSig:     randString(96, sigCharset),
				}
				if err := api.db.InsertAltSignAddr(ticketHash, data); err != nil {
					t.Fatalf("unable to insert alt sign addr: %v", err)
				}
			}

			w := httptest.NewRecorder()
			c, r := gin.CreateTestContext(w)

			var dcrdErr error
			if test.dcrdClientErr {
				dcrdErr = errors.New("error")
			}

			handle := func(c *gin.Context) {
				c.Set(dcrdKey, test.node)
				c.Set(dcrdErrorKey, dcrdErr)
				c.Set(requestBytesKey, b[test.deformReq:])
				api.setAltSignAddr(c)
			}

			r.POST("/", handle)

			c.Request, err = http.NewRequest(http.MethodPost, "/", nil)
			if err != nil {
				t.Fatal(err)
			}

			c.Request.Header.Set("VSP-Client-Signature", reqSig)

			r.ServeHTTP(w, c.Request)

			if test.wantHTTPStatus != w.Code {
				t.Fatalf("expected http status %d, got %d", test.wantHTTPStatus, w.Code)
			}

			if test.wantHTTPStatus != http.StatusOK {
				respBytes, err := io.ReadAll(w.Body)
				if err != nil {
					t.Fatalf("failed reading response body bytes: %v", err)
				}

				var apiError types.ErrorResponse
				err = json.Unmarshal(respBytes, &apiError)
				if err != nil {
					t.Fatalf("could not unmarshal error response: %v", err)
				}

				if test.wantErrCode != apiError.Code {
					t.Fatalf("incorrect error code, expected %d, actual %d",
						test.wantErrCode, apiError.Code)
				}

				if test.wantErrMsg != apiError.Message {
					t.Fatalf("incorrect error message, expected %q, actual %q",
						test.wantErrMsg, apiError.Message)
				}
			}

			altsig, err := api.db.AltSignAddrData(ticketHash)
			if err != nil {
				t.Fatalf("unable to get alt sign addr data: %v", err)
			}

			if test.wantHTTPStatus != http.StatusOK && !test.isExistingAltSignAddr {
				if altsig != nil {
					t.Fatalf("expected no alt sign addr saved for errored state")
				}
				return
			}

			if !bytes.Equal(b, []byte(altsig.Req)) || altsig.ReqSig != reqSig {
				t.Fatalf("expected alt sign addr data different than actual")
			}
		})
	}
}
