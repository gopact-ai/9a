package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestManagedSkillAutomaticAttachRepairAndDetach(t *testing.T) {
	if testing.Short() {
		t.Skip("process e2e")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(bin, "9a")
	build(t, cli, "./cmd/9a")
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	socket := socketPath(t)
	token := "managed-skills-admin"
	env := isolatedEnv(filepath.Join(root, "home"), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+token, "PATH="+bin+":"+os.Getenv("PATH"))
	var logs bytes.Buffer
	process := exec.Command(cli, "daemon", "--state", filepath.Join(root, "state.db"), "--socket", socket)
	process.Env = append(env, "NINEA_BOOTSTRAP_TOKEN="+token)
	process.Stderr = &logs
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	waitSocket(t, socket, &logs)
	command := exec.Command(cli, "search", "anything")
	command.Dir = workspace
	command.Env = env
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("automatic attach: %v\n%s", err, output)
	}
	skill := filepath.Join(workspace, ".agents", "skills", "using-ninea")
	info, err := os.Stat(filepath.Join(skill, "SKILL.md"))
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("SKILL.md mode=%v err=%v", info, err)
	}
	if err = os.Chmod(filepath.Join(skill, "SKILL.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	status := runInDir(t, workspace, env, cli, "", "status", "--json")
	if !bytes.Contains(status, []byte(`"state":"tampered"`)) || !bytes.Contains(status, []byte(`"mount_state":"tampered"`)) {
		t.Fatalf("tampered status=%s", status)
	}
	runInDir(t, workspace, env, cli, "", "update")
	data, err := os.ReadFile(filepath.Join(skill, "SKILL.md"))
	if err != nil || !bytes.Contains(data, []byte("# Using NineA")) {
		t.Fatalf("repair=%q err=%v", data, err)
	}
	runInDir(t, workspace, env, cli, "", "detach")
	if _, err = os.Stat(skill); !os.IsNotExist(err) {
		t.Fatalf("skill remains: %v", err)
	}
	status = runInDir(t, workspace, env, cli, "", "status", "--json")
	if !bytes.Contains(status, []byte(`"state":"detached"`)) {
		t.Fatalf("detached status=%s", status)
	}
	if err = process.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err = process.Wait(); err != nil {
		t.Fatalf("daemon stop: %v logs=%s", err, logs.String())
	}
}
