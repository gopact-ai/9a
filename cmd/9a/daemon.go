package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gopact-ai/9a/internal/api"
	"github.com/gopact-ai/9a/internal/app"
	"github.com/gopact-ai/9a/internal/authn"
	"github.com/gopact-ai/9a/internal/store"
	"github.com/spf13/cobra"
)

type localPaths struct {
	dir    string
	state  string
	socket string
	token  string
	log    string
	pid    string
	lock   string
}

const daemonStartupTimeout = 5 * time.Second

func localPathsForHome(home string) localPaths {
	dir := filepath.Join(home, ".local", "state", "ninea")
	return localPaths{
		dir:    dir,
		state:  filepath.Join(dir, "ninea.db"),
		socket: filepath.Join(dir, "ninea.sock"),
		token:  filepath.Join(dir, "admin-token"),
		log:    filepath.Join(dir, "daemon.log"),
		pid:    filepath.Join(dir, "daemon.pid"),
		lock:   filepath.Join(dir, "daemon.lock"),
	}
}

func daemonPaths(base localPaths, state, socket string) localPaths {
	paths := base
	paths.state = state
	paths.socket = socket
	if state != base.state {
		paths.token = state + ".admin-token"
		paths.log = state + ".log"
	}
	if socket != base.socket {
		paths.pid = socket + ".pid"
		paths.lock = socket + ".lock"
	}
	if state != base.state && socket != base.socket {
		paths.dir = ""
	}
	return paths
}

func defaultLocalPaths() (localPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return localPaths{}, fmt.Errorf("find home directory: %w", err)
	}
	return localPathsForHome(home), nil
}

func loadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return "", fmt.Errorf("secure token file %s: %w", path, err)
	}
	return token, nil
}

func loadOrCreateToken(path, explicit string) (string, error) {
	if explicit != "" {
		if strings.TrimSpace(explicit) != explicit {
			return "", errors.New("NINEA_BOOTSTRAP_TOKEN must not contain surrounding whitespace")
		}
		if err := os.WriteFile(path, []byte(explicit+"\n"), 0600); err != nil {
			return "", fmt.Errorf("write admin token file %s: %w", path, err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			return "", fmt.Errorf("secure admin token file %s: %w", path, err)
		}
		return explicit, nil
	}
	token, err := loadToken(path)
	if err == nil {
		return token, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("read admin token: %w", err)
	}
	token, err = authn.NewToken()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return loadToken(path)
	}
	if err != nil {
		return "", fmt.Errorf("create admin token file %s: %w", path, err)
	}
	if _, err := io.WriteString(file, token+"\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write admin token file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close admin token file %s: %w", path, err)
	}
	return token, nil
}

type daemonOptions struct {
	paths         localPaths
	state         string
	socket        string
	startupLockFD int
}

func prepareLocalPaths(options daemonOptions) error {
	if options.paths.dir != "" {
		if err := os.MkdirAll(options.paths.dir, 0700); err != nil {
			return fmt.Errorf("create local state directory: %w", err)
		}
		if err := os.Chmod(options.paths.dir, 0700); err != nil {
			return fmt.Errorf("secure local state directory: %w", err)
		}
	}
	for _, path := range []string{options.state, options.socket} {
		dir := filepath.Dir(path)
		if dir == "." || dir == options.paths.dir {
			continue
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

func daemonUnavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func acquireStartupLock(path, socket string, deadline time.Time) (*os.File, bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, false, fmt.Errorf("open daemon startup lock: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("secure daemon startup lock: %w", err)
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, false, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, false, fmt.Errorf("lock daemon startup: %w", err)
		}
		if socketAvailable(socket) {
			_ = file.Close()
			return nil, true, nil
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, false, fmt.Errorf("daemon startup lock timed out: %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func socketAvailable(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func startLocalDaemon(paths localPaths, socket string) error {
	paths = daemonPaths(paths, paths.state, socket)
	if err := prepareLocalPaths(daemonOptions{paths: paths, state: paths.state, socket: socket}); err != nil {
		return err
	}
	deadline := time.Now().Add(daemonStartupTimeout)
	lockFile, ready, err := acquireStartupLock(paths.lock, socket, deadline)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	defer lockFile.Close()
	if socketAvailable(socket) {
		return nil
	}
	logFile, err := os.OpenFile(paths.log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open daemon log %s: %w", paths.log, err)
	}
	defer logFile.Close()
	if err := os.Chmod(paths.log, 0600); err != nil {
		return fmt.Errorf("secure daemon log %s: %w", paths.log, err)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find 9a executable: %w", err)
	}
	cmd := exec.Command(executable, "daemon", "--socket", socket, "--internal-startup-lock-fd", "3")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{lockFile}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start 9a daemon: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("detach 9a daemon: %w", err)
	}
	return waitForSocket(socket, deadline)
}

func waitForSocket(socket string, deadline time.Time) error {
	for {
		if socketAvailable(socket) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon did not start at %s within %s", socket, daemonStartupTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func shutdown(ctx context.Context, closeHTTP, closeApp func(context.Context) error, closeDB func() error) error {
	return errors.Join(closeHTTP(ctx), closeApp(ctx), closeDB())
}

func runDaemon(parent context.Context, options daemonOptions) error {
	if err := prepareLocalPaths(options); err != nil {
		return err
	}
	var startupLock *os.File
	if options.startupLockFD > 0 {
		startupLock = os.NewFile(uintptr(options.startupLockFD), "daemon-startup-lock")
		if startupLock == nil {
			return errors.New("invalid inherited daemon startup lock")
		}
	} else {
		var ready bool
		var err error
		startupLock, ready, err = acquireStartupLock(
			options.paths.lock,
			options.socket,
			time.Now().Add(daemonStartupTimeout),
		)
		if err != nil {
			return err
		}
		if ready {
			return fmt.Errorf("daemon already running at %s", options.socket)
		}
		if socketAvailable(options.socket) {
			return fmt.Errorf("daemon already running at %s", options.socket)
		}
	}
	defer func() {
		if startupLock != nil {
			_ = startupLock.Close()
		}
	}()
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, options.state)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	a := app.New(db)
	listening := false
	defer func() {
		if !listening {
			_ = a.Close(context.Background())
			_ = db.Close()
		}
	}()
	if err := a.Restore(ctx); err != nil {
		return fmt.Errorf("restore state: %w", err)
	}
	bootstrap := os.Getenv("NINEA_BOOTSTRAP_TOKEN")
	needsBootstrap, err := a.NeedsBootstrap(ctx)
	if err != nil {
		return fmt.Errorf("check first start: %w", err)
	}
	if needsBootstrap {
		token, err := loadOrCreateToken(options.paths.token, bootstrap)
		if err != nil {
			return err
		}
		if err := a.Bootstrap(ctx, token); err != nil {
			return fmt.Errorf("bootstrap daemon: %w", err)
		}
		fmt.Fprintf(os.Stderr, "9a daemon initialized; admin token: %s\n", options.paths.token)
	} else if bootstrap != "" {
		return errors.New("NINEA_BOOTSTRAP_TOKEN must be unset after first start")
	}
	_ = os.Unsetenv("NINEA_BOOTSTRAP_TOKEN")
	_ = os.Unsetenv("NINEA_TOKEN")
	server, err := api.Listen(options.socket, a)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", options.socket, err)
	}
	if err := os.WriteFile(options.paths.pid, []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		_ = server.Close(context.Background())
		return fmt.Errorf("write daemon pid: %w", err)
	}
	defer os.Remove(options.paths.pid)
	listening = true
	if startupLock != nil {
		_ = startupLock.Close()
		startupLock = nil
	}
	fmt.Fprintf(os.Stderr, "9a daemon is running at %s\n", options.socket)
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return shutdown(shutdownCtx, server.Close, a.Close, db.Close)
}

func newDaemonCommand(paths localPaths) *cobra.Command {
	basePaths := paths
	options := daemonOptions{paths: paths, state: paths.state, socket: paths.socket}
	if socket := os.Getenv("NINEA_SOCKET"); socket != "" {
		options.socket = socket
	}
	cmd := &cobra.Command{
		Use:    "daemon",
		Short:  "Run the local background service",
		Long:   "Run the local service used by 9a commands. Normal commands start it automatically.",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if wantsJSON(cmd) {
				return fmt.Errorf("--json is not supported by the background daemon")
			}
			options.paths = daemonPaths(basePaths, options.state, options.socket)
			return runDaemon(cmd.Context(), options)
		},
	}
	cmd.Flags().StringVar(&options.state, "state", options.state, "SQLite state file")
	cmd.Flags().StringVar(&options.socket, "socket", options.socket, "Unix socket used by 9a")
	cmd.Flags().IntVar(&options.startupLockFD, "internal-startup-lock-fd", 0, "")
	_ = cmd.Flags().MarkHidden("internal-startup-lock-fd")
	return cmd
}
