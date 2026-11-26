package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

// configSaveObserver observes a committed setting while its wire output waits.
type configSaveObserver struct {
	observedList
	committed func()
}

func (s configSaveObserver) Save(ctx context.Context, sess store.Session, history, transcript []store.Row, cancelled func() bool) error {
	if sess.Model == "r" {
		s.committed()
	}
	return s.Store.Save(ctx, sess, history, transcript, cancelled)
}

func TestConfigOutputBackpressureKeepsReaderAndSessionFree(t *testing.T) {
	blocked, release := make(chan struct{}), make(chan struct{})
	committed, listed := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	c := startAgentWithOutput(t, t.TempDir(), nil, nil, func(w io.Writer) io.Writer {
		block := sync.OnceFunc(func() { close(blocked); <-release })
		return replayTestWriter(func(line []byte) (int, error) {
			if bytes.Contains(line, []byte(`"method":"_test/fill"`)) {
				block()
			}
			return w.Write(line)
		})
	}, func(st Store) Store {
		return configSaveObserver{observedList{st, listed}, sync.OnceFunc(func() { close(committed) })}
	})
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	wire := c.agent.deps.replays.wire
	if err := wire.Notify("_test/fill", nil); err != nil {
		t.Fatal(err)
	}
	issue50Wait(t, blocked)
	line, err := jsonrpc.NotificationLine("_test/fill", nil)
	if err != nil {
		t.Fatal(err)
	}
	for slot := wire.TryReserve(); slot != nil; slot = wire.TryReserve() {
		if err := slot.Enqueue(nil, [][]byte{line}, nil); err != nil {
			t.Fatal(err)
		}
	}

	set := func(configID, value string) <-chan rpcResponse {
		return c.send(acp.MethodSessionSetConfig, map[string]string{"sessionId": id, "configId": configID, "type": acp.ConfigValueID, "value": value})
	}
	first := set(configModel, "r")
	issue50Wait(t, committed)
	c.notify(jsonrpc.CancelRequestMethod, map[string]int{"requestId": c.lastID()})
	second := set(configEffort, "high")
	c.notify(jsonrpc.CancelRequestMethod, map[string]int{"requestId": c.lastID()})
	resumeParams := map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}}
	conflict := c.send(acp.MethodSessionResume, resumeParams)
	list := c.send(acp.MethodSessionList, map[string]any{})
	issue50Wait(t, listed) // The reader dispatched past the blocked setter.

	s, err := c.agent.session(id)
	if err != nil {
		t.Fatal(err)
	}
	captured := make(chan []acp.ConfigOption, 1)
	go func() { captured <- s.ConfigOptions() }()
	var options []acp.ConfigOption
	select {
	case options = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("setting still held the session lock while waiting for output")
	}
	if len(options) != 3 || options[0].CurrentValue != "r" || options[1].CurrentValue != "default" {
		t.Fatalf("settings while writer is blocked: %+v", options)
	}

	unblock()
	var result acp.SetConfigOptionResponse
	if r := c.await(first); r.Error != nil || json.Unmarshal(r.Result, &result) != nil || !reflect.DeepEqual(options, result.ConfigOptions) {
		t.Fatalf("committed setter was withdrawn: %+v", r)
	}
	if r := c.await(second); r.Error == nil || r.Error.Code != -32800 {
		t.Fatalf("queued setter was not cancelled: %+v", r)
	}
	if r := c.await(conflict); r.Error == nil || r.Error.Code != replayConflict {
		t.Fatalf("cursor did not see the output obligation: %+v", r)
	}
	if r := c.await(list); r.Error != nil {
		t.Fatal(r.Error)
	}
	c.ok(acp.MethodSessionResume, resumeParams, nil)
	updates := c.untilIdle(id)
	var pushed acp.ConfigOptionUpdate
	if len(updates) != 4 || updates[0].Update.Kind != "config_option_update" || updates[1].Update.Kind != "replay_complete" ||
		updates[2].Update.Kind != "session_info_update" || updates[3].Update.Kind != "state_update" ||
		json.Unmarshal(updates[0].Update.Raw, &pushed) != nil || !reflect.DeepEqual(options, pushed.ConfigOptions) {
		t.Fatalf("expected one committed snapshot before replay, got %+v", updates)
	}
}

type gatedConfigResumeFailure struct {
	Store
	entered chan struct{}
	release chan struct{}
}

func (s gatedConfigResumeFailure) Acquire(context.Context, string) (*store.Loaded, error) {
	close(s.entered)
	<-s.release
	return nil, errors.New("resume fixture failed")
}

func TestPromptAfterFailedResumeAndSuccessfulSetter(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	model := make(scriptedModel, 1)
	model.reply("done", llm.FinishStop, nil)
	c := startAgent(t, t.TempDir(), model, nil, func(st Store) Store {
		return gatedConfigResumeFailure{st, entered, release}
	})
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	c.selectModel(id, "m")
	s, err := c.agent.session(id)
	if err != nil {
		t.Fatal(err)
	}
	s.saves.Wait()
	s.mu.Lock()
	unlock := sync.OnceFunc(s.mu.Unlock)
	defer unlock()

	resume := c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd})
	issue50Wait(t, entered)
	c.agent.mu.Lock()
	failed := c.agent.operations[id]
	c.agent.mu.Unlock()
	set := c.send(acp.MethodSessionSetConfig, map[string]string{"sessionId": id, "configId": configModel, "type": acp.ConfigValueID, "value": "r"})
	c.fenceReader()
	unblock()
	if r := c.await(resume); r.Error == nil {
		t.Fatalf("expected setup failure: %+v", r)
	}

	issue50Wait(t, failed.done)
	prompt := c.prompt(id, "new prompt after the failed resume response")
	c.fenceReader()
	unlock()
	if r := c.await(set); r.Error != nil {
		t.Fatalf("model setter failed: %+v", r)
	}
	if r := c.await(prompt); r.Error != nil {
		t.Fatalf("prompt received a stale resume failure: %+v", r.Error)
	}
	c.untilIdle(id)
}

func TestActiveModeSetterSeesPendingWarmResume(t *testing.T) {
	entered, release, listed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var gateColdResponse atomic.Bool
	c := startAgentWithOutput(t, t.TempDir(), nil, nil, func(w io.Writer) io.Writer {
		return replayTestWriter(func(line []byte) (int, error) {
			if bytes.Contains(line, []byte(`"configOptions":`)) && gateColdResponse.CompareAndSwap(true, false) {
				close(entered)
				<-release
			}
			return w.Write(line)
		})
	}, func(st Store) Store { return observedList{st, listed} })
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	c.selectModel(id, "m")
	params := map[string]any{"sessionId": id, "cwd": cwd}
	c.ok(acp.MethodSessionClose, map[string]any{"sessionId": id}, nil)
	gateColdResponse.Store(true)
	cold := c.send(acp.MethodSessionResume, params)
	issue50Wait(t, entered)

	c.agent.mu.Lock()
	s := c.agent.sessions[id]
	c.agent.mu.Unlock()
	if s == nil || s.activation.Load() != activationWriting {
		t.Fatal("cold response write was not in progress")
	}
	s.mu.Lock()
	unlock := sync.OnceFunc(s.mu.Unlock)
	defer unlock()
	set := c.send(acp.MethodSessionSetConfig, map[string]string{"sessionId": id, "configId": configModel, "type": acp.ConfigValueID, "value": "r"})
	// A list handler can confirm receipt without needing the blocked writer.
	// The next input cannot be read until the setter handler has returned.
	list := c.send(acp.MethodSessionList, map[string]any{})
	issue50Wait(t, listed)
	c.agent.mu.Lock()
	staged := c.agent.operations[id]
	c.agent.mu.Unlock()
	if staged == nil || !staged.staged {
		t.Fatal("cold setter was not staged")
	}
	unblock()
	if r := c.await(cold); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := c.await(list); r.Error != nil {
		t.Fatal(r.Error)
	}
	if s.activation.Load() != activationActive {
		t.Fatal("cold response did not activate the session")
	}
	warm := c.send(acp.MethodSessionResume, params)
	mode := c.send(acp.MethodSessionSetConfig, map[string]string{"sessionId": id, "configId": configMode, "type": acp.ConfigValueID, "value": modePlan})
	c.fenceReader()
	unlock()
	if r := c.await(set); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := c.await(warm); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := c.await(mode); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("changed mode during pending warm resume: %+v", r)
	}
}
