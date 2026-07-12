package main

import (
	"context"
	"errors"
	"flag"
	"github.com/gopact-ai/9a/internal/api"
	"github.com/gopact-ai/9a/internal/app"
	"github.com/gopact-ai/9a/internal/store"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func shutdown(ctx context.Context, closeHTTP, closeApp func(context.Context) error, closeDB func() error) error {
	httpErr := closeHTTP(ctx)
	appErr := closeApp(ctx)
	dbErr := closeDB()
	return errors.Join(httpErr, appErr, dbErr)
}

func main() {
	state := flag.String("state", "ninea.db", "")
	socket := flag.String("socket", "/tmp/ninea.sock", "")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, e := store.Open(ctx, *state)
	if e != nil {
		log.Fatal(e)
	}
	a := app.New(db)
	if err := a.Restore(ctx); err != nil {
		log.Fatal(err)
	}
	bootstrap := os.Getenv("NINEA_BOOTSTRAP_TOKEN")
	needs, err := a.NeedsBootstrap(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if needs {
		if bootstrap == "" {
			log.Fatal("NINEA_BOOTSTRAP_TOKEN is required for first start")
		}
		if err := a.Bootstrap(ctx, bootstrap); err != nil {
			log.Fatal(err)
		}
	} else if bootstrap != "" {
		log.Fatal("NINEA_BOOTSTRAP_TOKEN must be unset after first start")
	}
	_ = os.Unsetenv("NINEA_BOOTSTRAP_TOKEN")
	_ = os.Unsetenv("NINEA_TOKEN")
	s, e := api.Listen(*socket, a)
	if e != nil {
		log.Fatal(e)
	}
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx, s.Close, a.Close, db.Close); err != nil {
		log.Print(err)
	}
}
