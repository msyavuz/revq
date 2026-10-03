// Package agent runs one-shot structured LLM calls. Claude Code is the only
// backend for now; anything that satisfies Runner can replace it.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Request struct {
	System     string
	Prompt     string
	Model      string
	Schema     string // JSON Schema the output must satisfy
	BudgetUSD  float64
	NoThinking bool // classification-style calls don't need it and it doubles output tokens
}

type Result struct {
	Output       json.RawMessage
	CostUSD      float64
	InputTokens  int
	OutputTokens int
}

type Runner interface {
	Run(ctx context.Context, req Request) (Result, error)
}

// ClaudeCode shells out to `claude -p`. The call is stripped down to keep the
// fixed overhead small: our own system prompt instead of the coding one, no
// tools, no MCP servers, no settings or CLAUDE.md, no session on disk. With
// no tools the model can only return text, so PR content can't make it act.
type ClaudeCode struct {
	Bin     string
	Timeout time.Duration
	dir     string
}

func NewClaudeCode(bin string) (*ClaudeCode, error) {
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("claude CLI not found (%s): %w", bin, err)
	}
	dir, err := os.MkdirTemp("", "revq-agent-")
	if err != nil {
		return nil, err
	}
	return &ClaudeCode{Bin: bin, Timeout: 10 * time.Minute, dir: dir}, nil
}

func (c *ClaudeCode) Run(ctx context.Context, req Request) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	args := []string{
		"-p",
		"--output-format", "json",
		"--model", req.Model,
		"--system-prompt", req.System,
		"--json-schema", req.Schema,
		"--tools", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--setting-sources", "",
	}
	if req.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(req.BudgetUSD, 'f', 2, 64))
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = c.dir
	if req.NoThinking {
		cmd.Env = append(os.Environ(), "MAX_THINKING_TOKENS=0")
	}
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	var resp struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		TotalCostUSD     float64         `json:"total_cost_usd"`
		Usage            struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		if runErr != nil {
			return Result{}, fmt.Errorf("claude: %w: %s", runErr, tail(stderr.String()+stdout.String()))
		}
		return Result{}, fmt.Errorf("claude: unparseable output: %s", tail(stdout.String()))
	}
	res := Result{
		CostUSD:      resp.TotalCostUSD,
		InputTokens:  resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}
	if resp.IsError || runErr != nil {
		msg := resp.Result
		if msg == "" {
			msg = resp.Subtype
		}
		return res, fmt.Errorf("claude: %s", tail(msg))
	}
	switch {
	case len(resp.StructuredOutput) > 0 && string(resp.StructuredOutput) != "null":
		res.Output = resp.StructuredOutput
	case json.Valid([]byte(resp.Result)):
		res.Output = json.RawMessage(resp.Result)
	default:
		return res, errors.New("claude: no structured output in response")
	}
	return res, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = "…" + s[len(s)-400:]
	}
	return s
}
