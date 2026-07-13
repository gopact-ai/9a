package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/store"
)

type callView struct {
	Call struct {
		State string `json:"state"`
		Code  string `json:"code"`
	} `json:"call"`
	Result json.RawMessage `json:"result"`
}

func waitCall(t *testing.T, env []string, cli, id string, states ...string) callView {
	t.Helper()
	wanted := map[string]bool{}
	for _, state := range states {
		wanted[state] = true
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		out := run(t, env, cli, "", "calls", "get", id, "--json")
		var view callView
		if err := json.Unmarshal(out, &view); err == nil && wanted[view.Call.State] {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("call %s did not reach %v: %s", id, states, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitSocket(t *testing.T, socket string, logs *bytes.Buffer) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(socket); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon socket: %s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecutableAsyncCallsCLIAndRestartRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("process e2e")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	_ = os.Mkdir(bin, 0755)
	cli, fixture := filepath.Join(bin, "9a"), filepath.Join(bin, "execfixture")
	build(t, cli, "./cmd/9a")
	build(t, fixture, "./testdata/executableadapter")
	socket := socketPath(t)
	state := filepath.Join(root, "state.db")
	token := "async-e2e-secret"
	pids := filepath.Join(root, "fixture-pids")
	adminEnv := isolatedEnv(filepath.Join(root, "home"), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+token, "NINEA_ASYNC_FIXTURE_PIDS="+pids)
	var logs bytes.Buffer
	startDaemon := func(bootstrap bool) *exec.Cmd {
		d := exec.Command(cli, "daemon", "--state", state, "--socket", socket)
		d.Env = adminEnv
		if bootstrap {
			d.Env = append(d.Env, "NINEA_BOOTSTRAP_TOKEN="+token)
		}
		d.Stderr = &logs
		if err := d.Start(); err != nil {
			t.Fatal(err)
		}
		waitSocket(t, socket, &logs)
		return d
	}
	d := startDaemon(true)
	t.Cleanup(func() { _ = d.Process.Kill(); _ = d.Wait() })
	run(t, adminEnv, cli, "", "adapters", "add", "exec", fixture)
	run(t, adminEnv, cli, "", "providers", "add", "exec", "demo", "local")
	agentToken := strings.TrimSpace(string(run(t, adminEnv, cli, "", "tokens", "create", "agent")))
	otherToken := strings.TrimSpace(string(run(t, adminEnv, cli, "", "tokens", "create", "other")))
	agentEnv := isolatedEnv(filepath.Join(root, "home"), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+agentToken)
	otherEnv := isolatedEnv(filepath.Join(root, "home"), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+otherToken)
	run(t, adminEnv, cli, "", "acl", "grant", "agent", "exec/demo/async", "invoke")
	completedID := strings.TrimSpace(string(run(t, agentEnv, cli, `{"block":false}`, "calls", "start", "exec/demo/async")))
	completed := waitCall(t, agentEnv, cli, completedID, "completed")
	if string(completed.Result) != `{"ok":true}` {
		t.Fatalf("completed=%#v", completed)
	}
	eventsOut := run(t, agentEnv, cli, "", "calls", "events", completedID, "--limit", "2", "--json")
	type eventPageView struct {
		Events []struct {
			Sequence int             `json:"sequence"`
			Envelope json.RawMessage `json:"envelope"`
		} `json:"events"`
		NextAfter int  `json:"next_after"`
		HasMore   bool `json:"has_more"`
	}
	var firstEvents eventPageView
	if err := json.Unmarshal(eventsOut, &firstEvents); err != nil || len(firstEvents.Events) != 2 || firstEvents.Events[0].Sequence != 1 || !bytes.Contains(firstEvents.Events[1].Envelope, []byte(`"encoding":"base64"`)) || firstEvents.NextAfter != 2 || !firstEvents.HasMore {
		t.Fatalf("events=%s err=%v", eventsOut, err)
	}
	eventsOut = run(t, agentEnv, cli, "", "calls", "events", completedID, "--after", strconv.Itoa(firstEvents.NextAfter), "--limit", "2", "--json")
	var secondEvents eventPageView
	if err := json.Unmarshal(eventsOut, &secondEvents); err != nil || len(secondEvents.Events) != 1 || secondEvents.Events[0].Sequence != 3 || secondEvents.NextAfter != 3 || secondEvents.HasMore {
		t.Fatalf("second events=%s err=%v", eventsOut, err)
	}
	if out := runFails(t, otherEnv, cli, "", "calls", "get", completedID); !bytes.Contains(out, []byte("call_not_found")) {
		t.Fatalf("other get=%s", out)
	}
	_ = run(t, adminEnv, cli, "", "calls", "get", completedID)
	cancelID := strings.TrimSpace(string(run(t, agentEnv, cli, `{"block":true}`, "calls", "start", "exec/demo/async")))
	_ = waitCall(t, agentEnv, cli, cancelID, "working")
	run(t, agentEnv, cli, "", "calls", "cancel", cancelID)
	canceled := waitCall(t, agentEnv, cli, cancelID, "canceled")
	if canceled.Call.Code != "canceled" {
		t.Fatalf("canceled=%#v", canceled)
	}
	persistenceID := strings.TrimSpace(string(run(t, agentEnv, cli, `{"block":true}`, "calls", "start", "exec/demo/async")))
	_ = waitCall(t, agentEnv, cli, persistenceID, "working")
	failureDB, err := store.Open(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_cli_cancel_terminal BEFORE UPDATE OF state ON calls WHEN OLD.id='%s' AND NEW.state IN ('canceled','failed') BEGIN SELECT RAISE(FAIL,'reject cli cancel terminal'); END`, persistenceID)
	if _, err := failureDB.Exec(trigger); err != nil {
		_ = failureDB.Close()
		t.Fatal(err)
	}
	_ = failureDB.Close()
	if out := runFails(t, agentEnv, cli, "", "calls", "cancel", persistenceID); !bytes.Contains(out, []byte("request_failed: call request failed")) {
		t.Fatalf("cancel persistence failure=%s", out)
	}
	if out := runFails(t, agentEnv, cli, "", "calls", "get", persistenceID); !bytes.Contains(out, []byte("request_failed: call request failed")) {
		t.Fatalf("get persistence failure=%s", out)
	}
	failureDB, err = store.Open(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failureDB.Exec(`DROP TRIGGER reject_cli_cancel_terminal`); err != nil {
		_ = failureDB.Close()
		t.Fatal(err)
	}
	_ = failureDB.Close()
	restartID := strings.TrimSpace(string(run(t, agentEnv, cli, `{"block":true}`, "calls", "start", "exec/demo/async")))
	_ = waitCall(t, agentEnv, cli, restartID, "working")
	if err := d.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = d.Wait()
	data, _ := os.ReadFile(pids)
	for _, line := range strings.Fields(string(data)) {
		pid, _ := strconv.Atoi(line)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = os.Remove(socket)
	d = startDaemon(false)
	recovered := waitCall(t, agentEnv, cli, restartID, "failed")
	if recovered.Call.Code != "daemon_restarted" {
		t.Fatalf("recovered=%#v", recovered)
	}
	persistenceRecovered := waitCall(t, agentEnv, cli, persistenceID, "failed")
	if persistenceRecovered.Call.Code != "daemon_restarted" {
		t.Fatalf("persistence recovered=%#v", persistenceRecovered)
	}
	completed = waitCall(t, agentEnv, cli, completedID, "completed")
	if string(completed.Result) != `{"ok":true}` {
		t.Fatalf("completed after restart=%#v", completed)
	}
}
