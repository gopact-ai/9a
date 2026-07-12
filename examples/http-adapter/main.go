package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const manifestEnvironment = "NINEA_HTTP_ADAPTER_MANIFEST"

func runAdapter(ctx context.Context, manifestPath string, input io.Reader, output, stderr io.Writer) error {
	if manifestPath == "" {
		return errors.New("NINEA_HTTP_ADAPTER_MANIFEST is required")
	}
	configuration, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}
	return newRuntime(configuration, input, output, stderr).serve(ctx)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = os.Stdin.Close()
	}()
	if err := runAdapter(ctx, os.Getenv(manifestEnvironment), os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "http-adapter: %v\n", err)
		os.Exit(2)
	}
}
