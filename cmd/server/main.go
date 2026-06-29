package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", envString("WACALLS_HTTP_ADDR", ":8080"), "HTTP listen address")
	dbPath := flag.String("db", envString("WACALLS_DB_PATH", "wacalls.db"), "SQLite session database path")
	staticDir := flag.String("static", envString("WACALLS_STATIC_DIR", "client/dist"), "static client directory (optional)")
	debug := flag.Bool("debug", false, "verbose logging")
	maxCalls := flag.Int("max-calls-per-session", envInt("WACALLS_MAX_CALLS_PER_SESSION", 8), "max concurrent calls per session (0 = unlimited)")
	asteriskTarget := flag.String("asterisk-sip-target", envString("WACALLS_ASTERISK_SIP_SERVER", envString("WACALLS_ASTERISK_SIP_TARGET", "")), "fallback Asterisk SIP server for inbound WhatsApp calls, e.g. asterisk:5060")
	asteriskFrom := flag.String("asterisk-sip-from", envString("WACALLS_ASTERISK_SIP_FROM", "wacalls"), "fallback SIP user")
	asteriskListen := flag.String("asterisk-sip-listen", envString("WACALLS_ASTERISK_SIP_LISTEN", envString("WACALLS_ASTERISK_SIP_BIND", "")), "local UDP listen address for INVITEs from Asterisk")
	asteriskBind := flag.String("asterisk-sip-bind", envString("WACALLS_ASTERISK_UAC_SIP_BIND", ":0"), "local UDP bind address for SIP client legs to Asterisk")
	asteriskRTPBind := flag.String("asterisk-rtp-bind", envString("WACALLS_ASTERISK_RTP_BIND", ":0"), "legacy local UDP bind address for RTP toward Asterisk")
	asteriskAdvertiseIP := flag.String("asterisk-advertise-ip", envString("WACALLS_ADVERTISE_IP", ""), "IP advertised in SIP Contact and SDP (auto-detected when empty)")
	asteriskRTPMin := flag.Int("asterisk-rtp-min", envInt("WACALLS_RTP_MIN", 0), "first RTP port for Asterisk media legs (0 = dynamic)")
	asteriskRTPMax := flag.Int("asterisk-rtp-max", envInt("WACALLS_RTP_MAX", 0), "last RTP port for Asterisk media legs")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	asterisk := AsteriskDefaults{
		DefaultTarget: *asteriskTarget,
		DefaultFrom:   *asteriskFrom,
		ListenBind:    *asteriskListen,
		SIPBind:       *asteriskBind,
		RTPBind:       *asteriskRTPBind,
		AdvertiseIP:   *asteriskAdvertiseIP,
		RTPMin:        *asteriskRTPMin,
		RTPMax:        *asteriskRTPMax,
	}
	srv, err := newServer(ctx, *dbPath, *staticDir, *maxCalls, asterisk, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer srv.sessions.disconnectAll()

	if err := srv.sessions.Restore(ctx); err != nil {
		log.Error("session restore failed", "err", err)
		os.Exit(1)
	}
	sipServer, err := srv.asterisk.StartSIPServer(ctx, srv.sessions, log)
	if err != nil {
		log.Error("asterisk sip server failed", "err", err)
		os.Exit(1)
	}
	defer sipServer.Close()

	httpSrv := &http.Server{Addr: *addr, Handler: srv.routes()}
	go func() {
		log.Info("HTTP server listening", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server error", "err", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
