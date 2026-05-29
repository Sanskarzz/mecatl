package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/stacklok/ozzharness/internal/adapter/openai"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
)

func main() {
	useOpenAI := flag.Bool("openai", false, "run against the live OpenAI Responses API (key from OPENAI_API_KEY)")
	model := flag.String("model", demoModel, "model identifier when --openai is set")
	baseURL := flag.String("openai-base-url", "", "override the OpenAI API base URL")
	flag.Parse()

	provider, label, err := selectProvider(*useOpenAI, *model, *baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ozzdemo:", err)
		os.Exit(1)
	}

	fmt.Printf("=== ozzharness demo (%s) ===\n", label)
	fmt.Println("Driving a real agent.Engine: auto-allowed tool call -> permission ask + approval -> final result.")
	fmt.Println()

	events, err := RunScenario(context.Background(), provider)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ozzdemo:", err)
		os.Exit(1)
	}

	for _, ev := range events {
		fmt.Println(formatEvent(ev))
	}
}

// selectProvider returns the configured LLMProvider and a human label. The
// offline mock is the default; --openai (with OPENAI_API_KEY) selects the live
// Responses adapter so the same scenario runs against a real model.
func selectProvider(useOpenAI bool, model, baseURL string) (port.LLMProvider, string, error) {
	if !useOpenAI {
		return mockProvider(), "offline / mockllm", nil
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, "", fmt.Errorf("--openai requires OPENAI_API_KEY to be set")
	}
	opts := []openai.Option{openai.WithAPIKey(key)}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...), "live / openai " + model, nil
}

// formatEvent renders one streamed Event as a single readable line so a human
// watching the terminal sees the loop, the tool call, the permission
// prompt+approval, and the final result + usage.
func formatEvent(ev session.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%03d] turn=%d %-14s", ev.Seq, ev.Turn, ev.Type)

	switch ev.Type {
	case session.EvMessageDelta:
		fmt.Fprintf(&b, " text=%q", ev.Text)
	case session.EvToolCall:
		if ev.ToolCall != nil {
			fmt.Fprintf(&b, " tool=%s args=%s", ev.ToolCall.Name, string(ev.ToolCall.Args))
		}
	case session.EvToolResult:
		if ev.ToolResult != nil {
			fmt.Fprintf(&b, " error=%t result=%q", ev.ToolResult.IsError, oneLine(ev.ToolResult.Content))
		}
	case session.EvPermissionAsk:
		if ev.Ask != nil {
			fmt.Fprintf(&b, " ASK tool=%s reason=%q  -> client auto-approves", ev.Ask.Tool, ev.Ask.Reason)
		}
	case session.EvHook:
		fmt.Fprintf(&b, " %s", ev.Text)
	case session.EvCompaction:
		fmt.Fprintf(&b, " summary=%q", oneLine(ev.Text))
	case session.EvResult:
		if ev.Result != nil {
			fmt.Fprintf(&b, " stop=%s text=%q", ev.Result.Stop, ev.Result.Text)
			u := ev.Result.Usage
			fmt.Fprintf(&b, "\n      usage: in=%d out=%d cacheRead=%d cacheWrite=%d cacheHitRate=%.2f",
				u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CacheHitRate())
		}
	}
	return b.String()
}

// oneLine collapses newlines so a multi-line tool result prints on a single row.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", " ⏎ ")
}
