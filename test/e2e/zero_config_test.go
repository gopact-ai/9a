package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConcurrentStatusStartsSingleLocalDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("process e2e")
	}

	root, err := os.MkdirTemp("/tmp", "ninea-zero-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cli := filepath.Join(root, "9a")
	build(t, cli, "./cmd/9a")
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(home, ".local", "state", "ninea")
	pidPath := filepath.Join(stateDir, "daemon.pid")
	t.Cleanup(func() {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return
		}
		process, _ := os.FindProcess(pid)
		_ = process.Signal(syscall.SIGTERM)
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(pidPath); os.IsNotExist(err) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = process.Kill()
	})

	env := isolatedEnv(home)
	commands := make([]*exec.Cmd, 10)
	outputs := make([]bytes.Buffer, len(commands))
	for i := range commands {
		commands[i] = exec.Command(cli, "status")
		commands[i].Dir = workspace
		commands[i].Env = env
		commands[i].Stdout = &outputs[i]
		commands[i].Stderr = &outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			log, _ := os.ReadFile(filepath.Join(stateDir, "daemon.log"))
			t.Fatalf("status %d: %v\n%s\ndaemon log:\n%s", i, err, outputs[i].Bytes(), log)
		}
		if got := outputs[i].String(); got != "Not ready\n  No integrations connected.\n  Next: 9a connect <manifest.yaml>\n" {
			t.Fatalf("status %d = %q, want Not ready", i, got)
		}
	}

	log, err := os.ReadFile(filepath.Join(stateDir, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(log, []byte("9a daemon is running")); got != 1 {
		t.Fatalf("daemon starts = %d, want 1\n%s", got, log)
	}
	for path, mode := range map[string]os.FileMode{
		stateDir:                               0o700,
		filepath.Join(stateDir, "admin-token"): 0o600,
		filepath.Join(stateDir, "ninea.sock"):  0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode = %v, error = %v; want %o", path, info, err, mode)
		}
	}
}
