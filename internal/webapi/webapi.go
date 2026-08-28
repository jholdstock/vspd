// Copyright (c) 2020-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package webapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/decred/slog"
	"github.com/decred/vspd/database"
	"github.com/decred/vspd/internal/config"
	"github.com/decred/vspd/rpc"
	"github.com/decred/vspd/types/v3"
	"github.com/dustin/go-humanize"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/sessions"
)

type Config struct {
	Listen               string
	VSPFee               float64
	Network              *config.Network
	FeeAccountName       string
	SupportEmail         string
	VspClosed            bool
	VspClosedMsg         string
	AdminPass            string
	Debug                bool
	Designation          string
	MaxVoteChangeRecords int
	VspdVersion          string
}

const (
	// feeAddressExpiration is the length of time a fee returned by /feeaddress
	// remains valid. After this time, a new fee must be requested.
	feeAddressExpiration = 1 * time.Hour
)

// Hard-coded keys used for storing values in the web context.
const (
	sessionKey           = "Session"
	dcrdKey              = "DcrdClient"
	dcrdHostKey          = "DcrdHostname"
	dcrdErrorKey         = "DcrdClientErr"
	cacheKey             = "Cache"
	walletsKey           = "WalletClients"
	failedWalletsKey     = "FailedWalletClients"
	requestBytesKey      = "RequestBytes"
	ticketKey            = "Ticket"
	knownTicketKey       = "KnownTicket"
	commitmentAddressKey = "CommitmentAddress"
)

type WebAPI struct {
	cfg           Config
	db            *database.VspDatabase
	log           slog.Logger
	addrGen       *addressGenerator
	cache         *cache
	adminPassHash [sha256.Size]byte
	signPrivKey   ed25519.PrivateKey
	signPubKey    ed25519.PublicKey
	server        *http.Server
	listener      net.Listener
}

func New(vdb *database.VspDatabase, log slog.Logger, dcrd rpc.DcrdConnect,
	wallets rpc.WalletConnect, cfg Config) (*WebAPI, error) {

	// Get keys for signing API responses from the database.
	signPrivKey, signPubKey, err := vdb.KeyPair()
	if err != nil {
		return nil, fmt.Errorf("db.Keypair error: %w", err)
	}

	// Populate cached VSP stats before starting webserver.
	encodedPubKey := base64.StdEncoding.EncodeToString(signPubKey)
	cache := newCache(encodedPubKey, log, vdb, dcrd, wallets)
	err = cache.update()
	if err != nil {
		log.Errorf("Could not initialize VSP stats cache: %v", err)
	}

	// Get the current fee xpub details from the database.
	feeXPub, err := vdb.FeeXPub()
	if err != nil {
		return nil, fmt.Errorf("db.FeeXPub error: %w", err)
	}

	// Use the retrieved pubkey to initialize an address generator which can
	// later be used to derive new fee addresses.
	addrGen, err := newAddressGenerator(feeXPub, cfg.Network.Params, log)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize fee address generator: %w", err)
	}

	// Get the secret key used to initialize the cookie store.
	cookieSecret, err := vdb.CookieSecret()
	if err != nil {
		return nil, fmt.Errorf("db.GetCookieSecret error: %w", err)
	}

	// Create TCP listener.
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}

	w := &WebAPI{
		cfg:           cfg,
		db:            vdb,
		log:           log,
		addrGen:       addrGen,
		cache:         cache,
		adminPassHash: sha256.Sum256([]byte(cfg.AdminPass)),
		signPrivKey:   signPrivKey,
		signPubKey:    signPubKey,
		listener:      listener,
	}

	w.server = &http.Server{
		Handler:      w.router(cookieSecret, dcrd, wallets),
		ReadTimeout:  5 * time.Second,  // slow requests should not hold connections opened
		WriteTimeout: 60 * time.Second, // hung responses must die
	}

	return w, nil
}

func (w *WebAPI) Run(ctx context.Context) {
	var wg sync.WaitGroup

	// Add the graceful shutdown to the waitgroup.
	wg.Go(func() {
		// Wait until context is canceled before shutting down the server.
		<-ctx.Done()

		w.log.Debug("Stopping webserver...")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		_ = w.server.Shutdown(shutdownCtx)
		cancel()

		w.log.Debug("Webserver stopped")
	})

	// Start webserver.
	wg.Go(func() {
		w.log.Infof("Listening on %s", w.listener.Addr())
		err := w.server.Serve(w.listener)
		// ErrServerClosed is expected from a graceful server shutdown, it can
		// be ignored. Anything else should be logged.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.log.Errorf("Unexpected webserver error: %v", err)
		}
	})

	// Periodically update cached VSP stats.
	wg.Go(func() {
		refresh := 1 * time.Minute
		if w.cfg.Debug {
			refresh = 1 * time.Second
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(refresh):
				err := w.cache.update()
				if err != nil {
					w.log.Errorf("Failed to update cached VSP stats: %v", err)
				}
			}
		}
	})

	wg.Wait()
}

func (w *WebAPI) router(cookieSecret []byte, dcrd rpc.DcrdConnect, wallets rpc.WalletConnect) *gin.Engine {
	// With release mode enabled, gin will only read template files once and cache them.
	// With release mode disabled, templates will be reloaded on the fly.
	if !w.cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()

	explorerURL := w.cfg.Network.BlockExplorerURL

	// Add custom functions for use in templates.
	router.SetFuncMap(template.FuncMap{
		"txURL":            txURL(explorerURL),
		"addressURL":       addressURL(explorerURL),
		"blockURL":         blockURL(explorerURL),
		"dateTime":         dateTime,
		"timeAgo":          timeAgo,
		"stripWss":         stripWss,
		"indentJSON":       indentJSON(w.log),
		"atomsToDCRString": atomsToDCRString,
		"atomsToDCRCoin":   atomsToDCRCoin,
		"float32ToPercent": float32ToPercent,
		"comma":            humanize.Comma,
		"pluralize":        pluralize,
	})

	router.LoadHTMLGlob("internal/webapi/templates/*.html")

	// Recovery middleware handles any go panics generated while processing web
	// requests. Ensures a 500 response is sent to the client rather than
	// sending no response at all.
	router.Use(recovery(w.log))

	if w.cfg.Debug {
		// Logger middleware outputs very detailed logging of webserver requests
		// to the terminal. Does not get logged to file.
		router.Use(gin.Logger())
	}

	router.Use(w.csrf())

	// Serve static web resources
	router.Static("/public", "internal/webapi/public/")

	// Create a cookie store for persisting admin session information.
	cookieStore := sessions.NewCookieStore(cookieSecret)

	// API routes.
	broadcastTicket := w.broadcastTicket()

	api := router.Group("/api/v3")
	api.GET("/vspinfo", w.requireWebCache, w.vspInfo)
	api.POST("/setaltsignaddr", w.vspMustBeOpen, w.withDcrdClient(dcrd), broadcastTicket, w.vspAuth, w.setAltSignAddr)
	api.POST("/feeaddress", w.vspMustBeOpen, w.withDcrdClient(dcrd), broadcastTicket, w.vspAuth, w.feeAddress)
	api.POST("/ticketstatus", w.withDcrdClient(dcrd), w.vspAuth, w.ticketStatus)
	api.POST("/payfee", w.vspMustBeOpen, w.withDcrdClient(dcrd), w.vspAuth, w.payFee)
	api.POST("/setvotechoices", w.withDcrdClient(dcrd), w.withWalletClients(wallets), w.vspAuth, w.setVoteChoices)

	// Website routes.

	router.GET("", w.requireWebCache, w.homepage)

	login := router.Group("/admin").Use(
		w.withSession(cookieStore),
	)

	// Limit login attempts to 3 per second.
	loginRateLmiter := rateLimit(3, func(c *gin.Context) {
		cacheData := c.MustGet(cacheKey).(cacheData)

		w.log.Warnf("Login rate limit exceeded by %s", c.ClientIP())
		c.HTML(http.StatusTooManyRequests, "login.html", gin.H{
			"WebApiCache":    cacheData,
			"WebApiCfg":      w.cfg,
			"FailedLoginMsg": "Rate limit exceeded",
		})
	})
	login.POST("", w.requireWebCache, loginRateLmiter, w.adminLogin)

	admin := router.Group("/admin").Use(
		w.requireWebCache,
		w.withWalletClients(wallets),
		w.withSession(cookieStore),
		w.requireAdmin)

	admin.GET("", w.withDcrdClient(dcrd), w.adminPage)
	admin.POST("/ticket", w.withDcrdClient(dcrd), w.ticketSearch)
	admin.GET("/backup", w.downloadDatabaseBackup)
	admin.POST("/logout", w.adminLogout)

	// Limit status endpoint attempts to 3 per second.
	statusRateLmiter := rateLimit(3, func(c *gin.Context) {
		w.log.Warnf("Status rate limit exceeded by %s", c.ClientIP())
		c.AbortWithStatus(http.StatusTooManyRequests)
	})

	// Require Basic HTTP Auth on /admin/status endpoint.
	basic := router.Group("/admin").Use(
		statusRateLmiter,
		w.withDcrdClient(dcrd),
		w.withWalletClients(wallets),
		gin.BasicAuth(gin.Accounts{
			"admin": w.cfg.AdminPass,
		}),
	)
	basic.GET("/status", w.statusJSON)

	return router
}

// sendJSONResponse serializes the provided response, signs it, and sends the
// response to the client with a 200 OK status. Returns the seralized response
// and the signature.
func (w *WebAPI) sendJSONResponse(resp any, c *gin.Context) (string, string) {
	dec, err := json.Marshal(resp)
	if err != nil {
		w.log.Errorf("JSON marshal error: %v", err)
		w.sendError(types.ErrInternalError, c)
		return "", ""
	}

	sig := ed25519.Sign(w.signPrivKey, dec)
	sigStr := base64.StdEncoding.EncodeToString(sig)
	c.Writer.Header().Set("VSP-Server-Signature", sigStr)

	c.AbortWithStatusJSON(http.StatusOK, resp)

	return string(dec), sigStr
}

// sendError sends an error response with the provided error code and the
// default message for that code.
func (w *WebAPI) sendError(e types.ErrorCode, c *gin.Context) {
	msg := e.DefaultMessage()
	w.sendErrorWithMsg(msg, e, c)
}

// sendErrorWithMsg sends an error response with the provided error code and
// message.
func (w *WebAPI) sendErrorWithMsg(msg string, e types.ErrorCode, c *gin.Context) {
	status := e.HTTPStatus()

	resp := types.ErrorResponse{
		Code:    e,
		Message: msg,
	}

	// Try to sign the error response. If it fails, send it without a signature.
	dec, err := json.Marshal(resp)
	if err != nil {
		w.log.Warnf("Sending error response without signature: %v", err)
	} else {
		sig := ed25519.Sign(w.signPrivKey, dec)
		c.Writer.Header().Set("VSP-Server-Signature", base64.StdEncoding.EncodeToString(sig))
	}

	c.AbortWithStatusJSON(status, resp)
}
