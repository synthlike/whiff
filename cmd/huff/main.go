// Command huff runs a model with tools, a turn at a time: on its own, a
// question and its answer; in a whiff job that holds an agent's session,
// a turn of that agent, whose tools are the whiff functions it may use.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/synthlike/whiff/internal/huff"
	"github.com/synthlike/whiff/internal/puff"
)

const usage = `usage: huff [flags] [prompt...]

Runs a model on prompt, or stdin if there is none, and prints its answer.
The model names its provider, with its key from the environment:

  openrouter/<model>   OpenRouter, as openrouter/moonshotai/kimi-k2: OPENROUTER_API_KEY
  openai/<model>       OpenAI: OPENAI_API_KEY
  local/<model>        a server on this machine at WHIFF_LOCAL_URL,
                       Ollama's http://localhost:11434/v1 if unset

It bills per token to those keys. In a whiff job that holds an agent's
session (WHIFF_SESSION), its tools are the whiff functions the agent may
use, called as its session: a :write waits for your review.

flags:
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

// run is huff with its arguments, streams and environment, returning the
// exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("huff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	model := fs.String("m", getenv("HUFF_MODEL"), "the model, as openrouter/moonshotai/kimi-k2; $HUFF_MODEL by default")
	system := fs.String("s", "", "the system prompt")
	transcript := fs.String("t", "", "a transcript file to go on from, if it is there, and to keep the turn in: puff data")
	calls := fs.Int("calls", 20, "the most model calls a turn makes")
	usd := fs.Float64("usd", 0, "the most dollars a turn spends, as the provider reports cost; 0 for no limit")
	tokens := fs.Int("tokens", 0, "the most tokens a turn uses, in and out; 0 for no limit")
	stream := fs.Bool("json", false, "print each step as a line of JSON, as claude -p --output-format stream-json does, the last saying how it ended")
	dir := fs.String("dir", "", "where its whiff tools act; the working directory by default")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "huff: "+format+"\n", a...)
		return 1
	}
	if *model == "" {
		return fail("name a model with -m, as openrouter/moonshotai/kimi-k2, or set HUFF_MODEL")
	}
	p, name, err := huff.Open(*model, getenv)
	if err != nil {
		return fail("%v", err)
	}
	input := strings.Join(fs.Args(), " ")
	if input == "" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fail("%v", err)
		}
		input = string(b)
	}
	if strings.TrimSpace(input) == "" {
		return fail("give a prompt, as arguments or on stdin")
	}
	history, err := readTranscript(*transcript)
	if err != nil {
		return fail("%v", err)
	}

	t := huff.Turn{Provider: p, Model: name, System: *system, History: history, Input: input,
		Limits: huff.Limits{Calls: *calls, USD: *usd, Tokens: *tokens}}
	out := json.NewEncoder(stdout)
	if *stream {
		out.Encode(map[string]any{"type": "system", "subtype": "init", "model": *model})
		t.Step = func(m huff.Message, _ huff.Usage) { out.Encode(streamStep(m)) }
	}
	if token := getenv("WHIFF_SESSION"); token != "" {
		wd := *dir
		if wd == "" {
			wd, _ = os.Getwd()
		}
		s, err := openSession(getenv("WHIFF_DIR"), token, wd)
		if err != nil {
			return fail("%v", err)
		}
		defer s.close()
		t.Tools = s
		t.System = strings.TrimSpace(toolsNote + "\n\n" + t.System)
	}

	r, err := huff.Run(ctx, t)
	if werr := writeTranscript(*transcript, r.History); werr != nil {
		fmt.Fprintf(stderr, "huff: the transcript isn't kept: %v\n", werr)
	}
	why := ""
	switch {
	case err != nil:
		why = err.Error()
	case r.Stop == "calls":
		why = fmt.Sprintf("the turn made its %d model calls and stopped", *calls)
	case r.Stop == "usd":
		why = fmt.Sprintf("Reached maximum budget ($%g)", *usd)
	case r.Stop == "tokens":
		why = fmt.Sprintf("the turn used its %d tokens and stopped", *tokens)
	case r.Stop == "length":
		why = "the model ran out of tokens for its answer"
	case r.Stop == "filter":
		why = "the provider withheld the model's answer"
	}
	if *stream {
		subtype := "success"
		switch {
		case err != nil:
			subtype = "error_during_execution"
		case r.Stop == "usd":
			subtype = "error_max_budget_usd"
		case r.Stop == "calls" || r.Stop == "tokens":
			subtype = "error_max_turns"
		case why != "":
			subtype = "error_" + r.Stop
		}
		res := map[string]any{"type": "result", "subtype": subtype, "is_error": why != "", "result": r.Text,
			"total_cost_usd": r.Usage.Cost, "num_turns": r.Calls,
			"usage": map[string]int{"input_tokens": r.Usage.Input, "output_tokens": r.Usage.Output}}
		if why != "" {
			res["errors"] = []string{why}
		}
		out.Encode(res)
	} else if r.Text != "" {
		fmt.Fprintln(stdout, strings.TrimSpace(r.Text))
	}
	if why != "" {
		if !*stream {
			fmt.Fprintf(stderr, "huff: %s\n", why)
		}
		return 1
	}
	return 0
}

// toolsNote tells a model in a whiff session how its tools behave.
const toolsNote = `Your tools are whiff functions. Their arguments are puff values written as strings: 12, :key, [1 2], {:a 1}, "text" with quotes; a JSON array or object reads as a vector or map; anything else that doesn't read as a value is taken as a string. A :read function answers at once. A :write one waits for the user to review it: you get a pending id at once. Don't wait for it: its result comes to you later, as a message of its own.`

// streamStep is m as a line of claude -p's stream-json, so whiff's
// tasks/watch shows it as it shows a Claude turn's.
func streamStep(m huff.Message) map[string]any {
	var content []map[string]any
	role := "assistant"
	switch m.Role {
	case "tool":
		role = "user"
		content = append(content, map[string]any{"type": "tool_result", "tool_use_id": m.ID, "content": m.Result, "is_error": m.IsError})
	default:
		if m.Text != "" {
			content = append(content, map[string]any{"type": "text", "text": m.Text})
		}
		for _, c := range m.Calls {
			var input any = c.Args
			if json.Valid([]byte(c.Args)) {
				input = json.RawMessage(c.Args)
			}
			content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Fn, "input": input})
		}
	}
	return map[string]any{"type": role, "message": map[string]any{"role": role, "content": content}}
}

// readTranscript is the transcript in path, none if path is "" or isn't
// there.
func readTranscript(path string) ([]huff.Message, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := puff.ReadValue(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	ms, err := huff.FromPuff(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return ms, nil
}

// writeTranscript keeps ms in path, whole or not at all, unless path is "".
func writeTranscript(path string, ms []huff.Message) error {
	if path == "" {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".huff-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.WriteString(tmp, puff.Print(huff.ToPuff(ms))+"\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
