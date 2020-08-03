package webapi

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/vspd/database"
	"github.com/decred/vspd/rpc"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/sessions"
)

type Config struct {
	VSPFee         float64
	NetParams      *chaincfg.Params
	FeeAccountName string
	SupportEmail   string
	VspClosed      bool
	AdminPass      string
	Debug          bool
	Designation    string
}

const (
	// TODO: Make this configurable or get it from RPC.
	relayFee = 0.0001
	// requiredConfs is the number of confirmations required to consider a
	// ticket purchase or a fee transaction to be final.
	requiredConfs = 6
	// feeAddressExpiration is the length of time a fee returned by /feeaddress
	// remains valid. After this time, a new fee must be requested.
	feeAddressExpiration = 1 * time.Hour
)

var cfg Config
var db *database.VspDatabase
var addrGen *addressGenerator
var signPrivKey ed25519.PrivateKey
var signPubKey ed25519.PublicKey

func Start(ctx context.Context, requestShutdownChan chan struct{}, shutdownWg *sync.WaitGroup,
	listen string, vdb *database.VspDatabase, dcrd rpc.DcrdConnect, wallets rpc.WalletConnect, config Config) error {

	cfg = config
	db = vdb

	var err error

	// Get keys for signing API responses from the database.
	signPrivKey, signPubKey, err = vdb.KeyPair()
	if err != nil {
		return fmt.Errorf("db.Keypair error: %v", err)
	}

	// Populate cached VSP stats before starting webserver.
	err = updateVSPStats(vdb, config)
	if err != nil {
		return fmt.Errorf("could not initialize VSP stats cache: %v", err)
	}

	// Get the last used address index and the feeXpub from the database, and
	// use them to initialize the address generator.
	idx, err := vdb.GetLastAddressIndex()
	if err != nil {
		return fmt.Errorf("db.GetLastAddressIndex error: %v", err)
	}
	feeXPub, err := vdb.GetFeeXPub()
	if err != nil {
		return fmt.Errorf("db.GetFeeXPub error: %v", err)
	}
	addrGen, err = newAddressGenerator(feeXPub, config.NetParams, idx)
	if err != nil {
		return fmt.Errorf("failed to initialize fee address generator: %v", err)
	}

	// Get the secret key used to initialize the cookie store.
	cookieSecret, err := vdb.GetCookieSecret()
	if err != nil {
		return fmt.Errorf("db.GetCookieSecret error: %v", err)
	}

	// Create TCP listener.
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", listen)
	if err != nil {
		return err
	}
	log.Infof("Listening on %s", listen)

	srv := http.Server{
		Handler:      router(cfg.Debug, cookieSecret, dcrd, wallets),
		ReadTimeout:  5 * time.Second,  // slow requests should not hold connections opened
		WriteTimeout: 60 * time.Second, // hung responses must die
	}

	// Add the graceful shutdown to the waitgroup.
	shutdownWg.Add(1)
	go func() {
		// Wait until shutdown is signaled before shutting down.
		<-ctx.Done()

		log.Debug("Stopping webserver...")
		// Give the webserver 5 seconds to finish what it is doing.
		timeoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(timeoutCtx); err != nil {
			log.Errorf("Failed to stop webserver cleanly: %v", err)
		} else {
			log.Debug("Webserver stopped")
		}
		shutdownWg.Done()
	}()

	// Start webserver.
	go func() {
		err = srv.Serve(listener)
		// If the server dies for any reason other than ErrServerClosed (from
		// graceful server.Shutdown), log the error and request vspd be
		// shutdown.
		if err != nil && err != http.ErrServerClosed {
			log.Errorf("Unexpected webserver error: %v", err)
			requestShutdownChan <- struct{}{}
		}
	}()

	// Use a ticker to update template data.
	var refresh time.Duration
	if cfg.Debug {
		refresh = 1 * time.Second
	} else {
		refresh = 1 * time.Minute
	}
	shutdownWg.Add(1)
	go func() {
		ticker := time.NewTicker(refresh)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				shutdownWg.Done()
				return
			case <-ticker.C:
				err = updateVSPStats(db, cfg)
				if err != nil {
					log.Errorf("Failed to update cached VSP stats: %v", err)
				}
			}
		}
	}()

	return nil
}

func router(debugMode bool, cookieSecret []byte, dcrd rpc.DcrdConnect, wallets rpc.WalletConnect) *gin.Engine {
	// With release mode enabled, gin will only read template files once and cache them.
	// With release mode disabled, templates will be reloaded on the fly.
	if !debugMode {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.LoadHTMLGlob("webapi/templates/*")

	// Recovery middleware handles any go panics generated while processing web
	// requests. Ensures a 500 response is sent to the client rather than
	// sending no response at all.
	router.Use(gin.Recovery())

	if debugMode {
		// Logger middleware outputs very detailed logging of webserver requests
		// to the terminal. Does not get logged to file.
		router.Use(gin.Logger())
	}

	// Serve static web resources
	router.Static("/public", "webapi/public/")

	// Create a cookie store for persisting admin session information.
	cookieStore := sessions.NewCookieStore(cookieSecret)

	// API routes.

	api := router.Group("/api/v3")
	api.GET("/vspinfo", vspInfo)
	api.POST("/feeaddress", withDcrdClient(dcrd), broadcastTicket(), vspAuth(), feeAddress)
	api.POST("/ticketstatus", withDcrdClient(dcrd), vspAuth(), ticketStatus)
	api.POST("/payfee", withDcrdClient(dcrd), vspAuth(), payFee)
	api.POST("/setvotechoices", withDcrdClient(dcrd), withWalletClients(wallets), vspAuth(), setVoteChoices)

	// Website routes.

	router.GET("", homepage)

	login := router.Group("/admin").Use(
		withSession(cookieStore),
	)
	login.POST("", adminLogin)

	admin := router.Group("/admin").Use(
		withWalletClients(wallets), withSession(cookieStore), requireAdmin(),
	)
	admin.GET("", adminPage)
	admin.POST("/ticket", ticketSearch)
	admin.GET("/backup", downloadDatabaseBackup)
	admin.POST("/logout", adminLogout)

	// Require Basic HTTP Auth on /admin/status endpoint.
	basic := router.Group("/admin").Use(
		withWalletClients(wallets), gin.BasicAuth(gin.Accounts{
			"admin": cfg.AdminPass,
		}),
	)
	basic.GET("/status", statusJSON)

	return router
}

func sendJSONResponse(resp interface{}, c *gin.Context) {
	dec, err := json.Marshal(resp)
	if err != nil {
		log.Errorf("JSON marshal error: %v", err)
		sendError(errInternalError, c)
		return
	}

	sig := ed25519.Sign(signPrivKey, dec)
	c.Writer.Header().Set("VSP-Server-Signature", hex.EncodeToString(sig))

	c.AbortWithStatusJSON(http.StatusOK, resp)
}

// sendError returns an error response to the client using the default error
// message.
func sendError(e apiError, c *gin.Context) {
	msg := e.defaultMessage()
	sendErrorWithMsg(msg, e, c)
}

// sendErrorWithMsg returns an error response to the client using the provided
// error message.
func sendErrorWithMsg(msg string, e apiError, c *gin.Context) {
	status := e.httpStatus()

	resp := gin.H{
		"code":    int(e),
		"message": msg,
	}

	// Try to sign the error response. If it fails, send it without a signature.
	dec, err := json.Marshal(resp)
	if err != nil {
		log.Warnf("Sending error response without signature: %v", err)
	} else {
		sig := ed25519.Sign(signPrivKey, dec)
		c.Writer.Header().Set("VSP-Server-Signature", hex.EncodeToString(sig))
	}

	c.AbortWithStatusJSON(status, resp)
}
