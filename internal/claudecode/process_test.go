package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeEnv switches the test binary into a fake `claude` (see TestMain):
// Start is pointed at os.Args[0], which then speaks just enough of the
// stream-json protocol to exercise Process without the real CLI.
const fakeEnv = "CLAUDECODE_FAKE_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		runFakeCLI()
		return
	}
	os.Exit(m.Run())
}

// runFakeCLI answers each stdin message with a text delta, one assistant
// text block, a tool_result echo and a result. Special texts: "hang" never
// answers, "crash" exits non-zero with a stderr message.
func runFakeCLI() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var msg struct {
			Message struct {
				Content []map[string]any `json:"content"`
			} `json:"message"`
		}
		_ = json.Unmarshal(in.Bytes(), &msg)
		var text string
		for _, c := range msg.Message.Content {
			if c["type"] == "text" {
				text, _ = c["text"].(string)
			}
		}
		switch text {
		case "hang":
			time.Sleep(time.Hour)
		case "crash":
			fmt.Fprintln(os.Stderr, "boom: not logged in")
			os.Exit(3)
		}
		reply := "echo: " + text
		_ = out.Encode(map[string]any{"type": "system", "subtype": "init"})
		_ = out.Encode(map[string]any{"type": "stream_event", "event": map[string]any{
			"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": reply}}})
		_ = out.Encode(map[string]any{"type": "assistant", "message": map[string]any{
			"id": "msg_1", "content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "mcp__miranda__x", "input": map[string]any{"a": 1}}}}})
		_ = out.Encode(map[string]any{"type": "user", "message": map[string]any{
			"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}}})
		_ = out.Encode(map[string]any{"type": "assistant", "message": map[string]any{
			"id": "msg_2", "content": []any{map[string]any{"type": "text", "text": reply}}}})
		_ = out.Encode(map[string]any{"type": "result", "subtype": "success", "result": reply, "num_turns": 2, "session_id": "s1"})
	}
}

func startFake(t *testing.T) *Process {
	t.Helper()
	t.Setenv(fakeEnv, "1")
	p, err := Start(Options{Binary: os.Args[0], SystemPrompt: "secret prompt"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestTurnRoundTripsAndKeepsProcess(t *testing.T) {
	p := startFake(t)
	for _, text := range []string{"привет", "ещё раз"} {
		var deltas string
		var blocks []string
		var toolResults int
		res, err := p.Turn(context.Background(), UserMessage{Text: text}, Handler{
			OnTextDelta:   func(d string) { deltas += d },
			OnAssistant:   func(id string, b ContentBlock) { blocks = append(blocks, id+":"+b.Type) },
			OnToolResults: func(bs []ContentBlock) { toolResults += len(bs) },
		})
		if err != nil {
			t.Fatalf("Turn(%q): %v", text, err)
		}
		want := "echo: " + text
		if res.Text != want || deltas != want || res.Subtype != "success" || res.NumTurns != 2 {
			t.Fatalf("Turn(%q) = %+v, deltas %q", text, res, deltas)
		}
		if !slices.Equal(blocks, []string{"msg_1:tool_use", "msg_2:text"}) || toolResults != 1 {
			t.Fatalf("blocks %v, toolResults %d", blocks, toolResults)
		}
	}
	if !p.Alive() {
		t.Fatal("process should survive between turns")
	}
}

func TestTurnContextCancelKillsProcess(t *testing.T) {
	p := startFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.Turn(ctx, UserMessage{Text: "hang"}, Handler{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Alive() {
		t.Fatal("process should be killed after a cancelled turn")
	}
}

func TestTurnReportsExitWithStderr(t *testing.T) {
	p := startFake(t)
	_, err := p.Turn(context.Background(), UserMessage{Text: "crash"}, Handler{})
	if !errors.Is(err, ErrProcessExited) || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err = %v, want ErrProcessExited with stderr tail", err)
	}
}

func TestCloseRemovesWorkDir(t *testing.T) {
	p := startFake(t)
	if _, err := os.Stat(p.dir); err != nil {
		t.Fatalf("work dir missing while running: %v", err)
	}
	_ = p.Close()
	if _, err := os.Stat(p.dir); !os.IsNotExist(err) {
		t.Fatalf("work dir should be removed, stat err = %v", err)
	}
}

func TestBuildArgsIsolatesTheProcess(t *testing.T) {
	args := buildArgs(Options{Model: "haiku", MaxTurns: 7, BuiltinTools: []string{"WebSearch", "WebFetch"}, AllowedTools: []string{"mcp__miranda__*"}, ExtraArgs: []string{"--x"}}, "/p", "/m")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--input-format stream-json", "--output-format stream-json", "--no-session-persistence",
		"--strict-mcp-config", "--mcp-config /m", "--disable-slash-commands", "--system-prompt-file /p",
		"--tools WebSearch,WebFetch", "--allowedTools WebSearch,WebFetch,mcp__miranda__*", "--model haiku", "--max-turns 7",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if none := buildArgs(Options{}, "/p", "/m"); slices.Index(none, "--tools") < 0 || none[slices.Index(none, "--tools")+1] != "" {
		t.Errorf("no BuiltinTools must pass an empty --tools (all built-ins off): %v", none)
	}
	if i := slices.Index(args, "--setting-sources"); i < 0 || args[i+1] != "" {
		t.Errorf("--setting-sources must be followed by an empty value: %v", args)
	}
	if slices.Contains(args, "--bare") {
		t.Error("--bare must not be used: it never reads the subscription login")
	}
	if args[len(args)-1] != "--x" {
		t.Errorf("ExtraArgs must come last: %v", args)
	}
}

func TestChildEnvStripsAPIKeys(t *testing.T) {
	env := []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk", "ANTHROPIC_AUTH_TOKEN=t", "CLAUDECODE=1", "HOME=/h"}
	if got := childEnv(env, false); !slices.Equal(got, []string{"PATH=/bin", "HOME=/h"}) {
		t.Fatalf("childEnv(false) = %v", got)
	}
	if got := childEnv(env, true); !slices.Equal(got, []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk", "ANTHROPIC_AUTH_TOKEN=t", "HOME=/h"}) {
		t.Fatalf("childEnv(true) = %v", got)
	}
}
