// This file implements `miranda llm-trace` — the same diagnostic reader for
// logs/llm.log that miranda-medical-card's own `medical-dev llm-trace`
// provides, both now thin wrappers around miranda-llm/llmtrace/analyze (see
// that package's doc comment for the block format and why the parsing logic
// moved there): one "=== <time> provider=<name> [conversation=<id>] ==="
// section per Router.Chat/Structured call, with the exact request/response
// each provider sent (see run's own llmTracer wiring in main.go).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/archer-developer/miranda-llm/llmtrace/analyze"
	"github.com/archer-developer/miranda/internal/config"
)

func runLLMTrace(cfg config.LoggingConfig, args []string) error {
	fs := flag.NewFlagSet("llm-trace", flag.ExitOnError)
	file := fs.String("file", filepath.Join(cfg.Dir, "llm.log"), "path to the LLM trace log")
	conversation := fs.String("conversation", "", "show every turn of this conversation id")
	latest := fs.Bool("latest", false, "show every turn of the most recently started conversation")
	untagged := fs.Bool("untagged", false, "show the most recent untagged (stateless) turns")
	if err := fs.Parse(args); err != nil {
		return err
	}

	f, err := os.Open(*file)
	if err != nil {
		return fmt.Errorf("open trace log %s: %w", *file, err)
	}
	defer f.Close()

	blocks, err := analyze.ParseAll(f)
	if err != nil {
		return fmt.Errorf("parse trace log %s: %w", *file, err)
	}

	if *untagged {
		const untaggedTail = 20
		return analyze.RenderUntagged(os.Stdout, blocks, untaggedTail)
	}

	if *latest {
		id := analyze.LatestConversation(blocks)
		if id == "" {
			return fmt.Errorf("no conversation-tagged blocks found in %s", *file)
		}
		*conversation = id
	}

	if *conversation == "" {
		analyze.RenderConversationSummary(os.Stdout, blocks)
		return nil
	}
	return analyze.RenderConversationDetail(os.Stdout, blocks, *conversation)
}
