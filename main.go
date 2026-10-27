package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() { os.Exit(execute()) }
func execute() int {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: watchlight validate|serve|watchdog [-config config.yaml]")
		return 2
	}
	mode := os.Args[1]
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	path := flags.String("config", "config.yaml", "configuration file")
	if flags.Parse(os.Args[2:]) != nil {
		return 2
	}
	if mode != "validate" && mode != "serve" && mode != "watchdog" {
		log.Print("unknown command")
		return 2
	}
	c, e := loadConfig(*path)
	if e != nil {
		log.Printf("configuration: %v", e)
		return 1
	}
	if mode == "validate" {
		fmt.Printf("Configuration valid: %d monitors\n", len(c.Monitors))
		return 0
	}
	required := []string{}
	if mode == "serve" {
		required = append(required, c.OwnerPasswordEnv, c.Telegram.TokenEnv, c.Telegram.ChatIDEnv)
		for _, m := range c.Monitors {
			required = append(required, m.TokenEnv)
		}
	} else {
		if c.Watchdog == nil {
			log.Print("watchdog section required")
			return 1
		}
		required = append(required, c.Watchdog.PasswordEnv, c.Watchdog.TelegramTokenEnv)
		if c.Watchdog.SMTP.User != "" {
			required = append(required, c.Watchdog.SMTP.PasswordEnv)
		}
	}
	for _, name := range required {
		if name != "" {
			if _, e = secret(name); e != nil {
				log.Print(e)
				return 1
			}
		}
	}
	if mode == "serve" {
		p, _ := secret(c.OwnerPasswordEnv)
		if len(p) < 16 {
			log.Print("owner password must be at least 16 characters")
			return 1
		}
	}
	// Files created by SQLite and its WAL inherit restrictive process permissions.
	syscall.Umask(0077)
	s, e := openStore(c.Database)
	if e != nil {
		log.Printf("database: %v", e)
		return 1
	}
	defer s.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if mode == "watchdog" {
		if e = runWatchdog(ctx, c, s); e != nil {
			log.Print(e)
			return 1
		}
		return 0
	}
	waitMonitors := runMonitors(ctx, c, s)
	notifierDone := make(chan struct{})
	go func() { defer close(notifierDone); runNotifications(ctx, c, s) }()
	defer func() { stop(); waitMonitors(); <-notifierDone }()
	srv := &http.Server{Addr: c.Listen, Handler: handler(c, s), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("Watchlight listening on %s (%s)", c.Listen, strings.Join([]string{"read-only UI", "SQLite"}, ", "))
	if e = srv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		log.Print(e)
		return 1
	}
	return 0
}
