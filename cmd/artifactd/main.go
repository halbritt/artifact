package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/halbritt/artifact/internal/artifact"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8879", "HTTP bind address")
	data := flag.String("data", "data", "artifact store directory")
	base := flag.String("base-url", artifact.DefaultURL, "public URL origin")
	flag.Parse()
	store, err := artifact.NewStore(*data, *base)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: store.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	stop, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer done()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-stop.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("artifact serving %s at %s", *data, *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}
