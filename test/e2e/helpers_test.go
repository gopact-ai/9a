package e2e

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func build(t *testing.T, out, pkg string) {
	t.Helper()
	command := exec.Command("go", "build", "-o", out, pkg)
	command.Dir = "../.."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, output)
	}
}

func isolatedEnv(home string, overrides ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides)+1)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "NINEA_") || strings.HasPrefix(value, "HOME=") {
			continue
		}
		env = append(env, value)
	}
	return append(env, append([]string{"HOME=" + home}, overrides...)...)
}

func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ninea-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "daemon.sock")
}

func cleanupReadOnlyDirectories(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
}

func waitSocket(t *testing.T, socket string, logs *lockedBuffer) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon socket: %s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
