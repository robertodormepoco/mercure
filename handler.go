package mercure

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/unrolled/secure"
	"go.uber.org/zap"
	"golang.org/x/crypto/acme/autocert"
)

const (
	defaultHubURL  = "/.well-known/mercure"
	defaultUIURL   = defaultHubURL + "/ui/"
	defaultDemoURL = defaultUIURL + "demo/"
)

func (h *Hub) initHandler() {
	router := mux.NewRouter()
	router.UseEncodedPath()
	router.SkipClean(true)

	csp := "default-src 'self'"
	if h.demo {
		router.PathPrefix(defaultDemoURL).HandlerFunc(Demo).Methods("GET", "HEAD")
	}
	if h.ui {
		public, err := fs.Sub(uiContent, "public")
		if err != nil {
			panic(err)
		}

		router.PathPrefix(defaultUIURL).Handler(http.StripPrefix(defaultUIURL, http.FileServer(http.FS(public))))

		csp += " mercure.rocks cdn.jsdelivr.net"
	}

	h.registerSubscriptionHandlers(router)

	router.HandleFunc(defaultHubURL, h.SubscribeHandler).Methods("GET", "HEAD")
	router.HandleFunc(defaultHubURL, h.PublishHandler).Methods("POST")

	secureMiddleware := secure.New(secure.Options{
		IsDevelopment:         h.debug,
		AllowedHosts:          h.allowedHosts,
		FrameDeny:             true,
		ContentTypeNosniff:    true,
		BrowserXssFilter:      true,
		ContentSecurityPolicy: csp,
	})

	if len(h.corsOrigins) == 0 {
		h.handler = secureMiddleware.Handler(router)

		return
	}

	h.handler = secureMiddleware.Handler(
		handlers.CORS(
			handlers.AllowCredentials(),
			handlers.AllowedOrigins(h.corsOrigins),
			handlers.AllowedHeaders([]string{"authorization", "cache-control"}),
		)(router),
	)
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

// Serve starts the HTTP server.
//
// Deprecated: use the Caddy server module or the standalone library instead.
func (h *Hub) Serve() {
	addr := h.config.GetString("addr")

	h.server = &http.Server{
		Addr:         addr,
		Handler:      h.baseHandler(),
		ReadTimeout:  h.config.GetDuration("read_timeout"),
		WriteTimeout: h.config.GetDuration("write_timeout"),
	}

	if _, ok := h.metrics.(*PrometheusMetrics); ok {
		addr := h.config.GetString("metrics_addr")

		h.metricsServer = &http.Server{
			Addr:    addr,
			Handler: h.metricsHandler(),
		}

		h.logger.Info("Mercure metrics started", zap.String("addr", addr))
		go h.metricsServer.ListenAndServe()
	}

	acme := len(h.allowedHosts) > 0
	certFile := h.config.GetString("cert_file")
	keyFile := h.config.GetString("key_file")

	done := h.listenShutdown()
	var err error

	if !acme && certFile == "" && keyFile == "" {
		h.logger.Info("Mercure started", zap.String("protocol", "http"), zap.String("addr", addr))
		err = h.server.ListenAndServe()
	} else {
		// TLS
		if acme {
			certManager := &autocert.Manager{
				Prompt:     autocert.AcceptTOS,
				HostPolicy: autocert.HostWhitelist(h.allowedHosts...),
			}

			acmeCertDir := h.config.GetString("acme_cert_dir")
			if acmeCertDir != "" {
				certManager.Cache = autocert.DirCache(acmeCertDir)
			}
			h.server.TLSConfig = certManager.TLSConfig()

			// Mandatory for Let's Encrypt http-01 challenge
			go http.ListenAndServe(h.config.GetString("acme_http01_addr"), certManager.HTTPHandler(nil))
		}

		h.logger.Info("Mercure started", zap.String("protocol", "https"), zap.String("addr", addr))
		err = h.server.ListenAndServeTLS(certFile, keyFile)
	}

	if !errors.Is(err, http.ErrServerClosed) {
		h.logger.Error("Unexpected error", zap.Error(err))
	}

	<-done
}

// Deprecated: use the Caddy server module or the standalone library instead.
func (h *Hub) listenShutdown() <-chan struct{} {
	idleConnsClosed := make(chan struct{})

	h.server.RegisterOnShutdown(func() {
		select {
		case <-idleConnsClosed:
		default:
			close(idleConnsClosed)
		}
	})

	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)
		<-sigint

		if err := h.server.Shutdown(context.Background()); err != nil {
			h.logger.Error("Unexpected error during server shutdown", zap.Error(err))
		}
		if err := h.metricsServer.Shutdown(context.Background()); err != nil {
			h.logger.Error("Unexpected error during metrics server shutdown", zap.Error(err))
		}
		h.logger.Info("My Baby Shot Me Down")

		select {
		case <-idleConnsClosed:
		default:
			close(idleConnsClosed)
		}
	}()

	return idleConnsClosed
}

// chainHandlers configures and chains handlers.
func (h *Hub) chainHandlers() http.Handler { //nolint:funlen
	r := mux.NewRouter()
	h.registerSubscriptionHandlers(r)

	r.HandleFunc(defaultHubURL, h.SubscribeHandler).Methods("GET", "HEAD")
	r.HandleFunc(defaultHubURL, h.PublishHandler).Methods("POST")

	csp := "default-src 'self'"
	if h.demo {
		r.PathPrefix("/demo").HandlerFunc(Demo).Methods("GET", "HEAD")
	}

	if h.ui {
		public, err := fs.Sub(uiContent, "public")
		if err != nil {
			panic(err)
		}

		r.PathPrefix("/").Handler(http.FileServer(http.FS(public)))
		csp += " mercure.rocks cdn.jsdelivr.net"
	} else {
		r.HandleFunc("/", welcomeHandler).Methods("GET", "HEAD")
	}

	secureMiddleware := secure.New(secure.Options{
		IsDevelopment:         h.debug,
		AllowedHosts:          h.allowedHosts,
		FrameDeny:             true,
		ContentTypeNosniff:    true,
		BrowserXssFilter:      true,
		ContentSecurityPolicy: csp,
	})

	var corsHandler http.Handler
	if len(h.corsOrigins) > 0 {
		allowedOrigins := handlers.AllowedOrigins(h.corsOrigins)
		allowedHeaders := handlers.AllowedHeaders([]string{"authorization", "cache-control"})

		corsHandler = handlers.CORS(handlers.AllowCredentials(), allowedOrigins, allowedHeaders)(r)
	} else {
		corsHandler = r
	}

	var compressHandler http.Handler
	if h.config.GetBool("compress") {
		compressHandler = handlers.CompressHandler(corsHandler)
	} else {
		compressHandler = corsHandler
	}

	var useForwardedHeadersHandlers http.Handler
	if h.config.GetBool("use_forwarded_headers") {
		useForwardedHeadersHandlers = handlers.ProxyHeaders(compressHandler)
	} else {
		useForwardedHeadersHandlers = compressHandler
	}

	secureHandler := secureMiddleware.Handler(useForwardedHeadersHandlers)
	loggingHandler := handlers.CombinedLoggingHandler(os.Stderr, secureHandler)
	recoveryHandler := handlers.RecoveryHandler(
		handlers.RecoveryLogger(zapRecoveryHandlerLogger{h.logger}),
		handlers.PrintRecoveryStack(h.debug),
	)(loggingHandler)

	return recoveryHandler
}

func (h *Hub) registerSubscriptionHandlers(r *mux.Router) {
	if !h.subscriptions {
		return
	}
	if _, ok := h.transport.(TransportSubscribers); !ok {
		h.logger.Error("The current transport doesn't support subscriptions. Subscription API disabled.")

		return
	}

	r.UseEncodedPath()
	r.SkipClean(true)

	r.HandleFunc(subscriptionURL, h.SubscriptionHandler).Methods("GET")
	r.HandleFunc(subscriptionsForTopicURL, h.SubscriptionsHandler).Methods("GET")
	r.HandleFunc(subscriptionsURL, h.SubscriptionsHandler).Methods("GET")
}

// Deprecated: use the Caddy server module or the standalone library instead.
func (h *Hub) baseHandler() http.Handler {
	mainRouter := mux.NewRouter()
	mainRouter.UseEncodedPath()
	mainRouter.SkipClean(true)

	// Register /healthz (if enabled, in a way that doesn't pollute the HTTP logs).
	registerHealthz(mainRouter)

	handler := h.chainHandlers()
	mainRouter.PathPrefix("/").Handler(handler)

	return mainRouter
}

// Deprecated: use the Caddy server module or the standalone library instead.
func (h *Hub) metricsHandler() http.Handler {
	router := mux.NewRouter()

	registerHealthz(router)
	h.metrics.(*PrometheusMetrics).Register(router.PathPrefix("/").Subrouter())

	return router
}

// Deprecated: use the Caddy server module or the standalone library instead.
func registerHealthz(router *mux.Router) {
	router.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}).Methods("GET", "HEAD")
}

// Deprecated: use the Caddy server module or the standalone library instead.
func welcomeHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, `<!DOCTYPE html>
<title>Mercure Hub</title>
<h1>Welcome to <a href="https://mercure.rocks">Mercure</a>!</h1>`)
}

// Deprecated: use the Caddy server module or the standalone library instead.
type zapRecoveryHandlerLogger struct {
	logger Logger
}

func (z zapRecoveryHandlerLogger) Println(args ...interface{}) {
	z.logger.Error(fmt.Sprint(args...))
}
