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
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "wacalls.db", "SQLite session database path")
	staticDir := flag.String("static", "client/dist", "static client directory (optional)")
	debug := flag.Bool("debug", false, "verbose logging")
	maxCalls := flag.Int("max-calls-per-session", 8, "max concurrent calls per session (0 = unlimited)")
	asteriskTarget := flag.String("asterisk-sip-target", "", "SIP target for inbound WhatsApp calls, e.g. 600@asterisk:5060")
	asteriskFrom := flag.String("asterisk-sip-from", "wacalls", "SIP user used in From/Contact headers")
	asteriskBind := flag.String("asterisk-sip-bind", ":0", "local UDP bind address for SIP")
	asteriskRTPBind := flag.String("asterisk-rtp-bind", ":0", "local UDP bind address for RTP toward Asterisk")
	asteriskAdvertiseIP := flag.String("asterisk-advertise-ip", "", "IP advertised in SIP Contact and SDP (auto-detected when empty)")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	asterisk := SIPConfig{
		Target:      *asteriskTarget,
		FromUser:    *asteriskFrom,
		Bind:        *asteriskBind,
		RTPBind:     *asteriskRTPBind,
		AdvertiseIP: *asteriskAdvertiseIP,
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
