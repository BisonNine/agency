package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ergochat/readline"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/utils"
)

// cliSessionKey is the session key `claw agent` sends with each message. The
// loop routes every message to the agent's one conversation (agent:<id>:main)
// whatever key it carries.
const cliSessionKey = "cli:default"

// pruneHumanProblems applies the human-agent rules the gateway applies to its
// running copy (config.PruneHumanProblems): an agent breaking them is not run
// here either.
func pruneHumanProblems(cfg *config.Config) {
	for _, p := range cfg.PruneHumanProblems() {
		logger.WarnCF("agent", "ignored human-agent configuration", map[string]any{
			"agent": p.Agent, "model": p.Model, "reason": p.Message,
		})
	}
}

func agentCmd(message, model string, debug bool) error {
	if debug {
		logger.SetLevel(logger.DEBUG)
		logger.DebugC("agent", "Debug mode enabled")
	}

	cfg, err := internal.LoadConfig()
	if err != nil {
		return fmt.Errorf("error loading config: %w", err)
	}

	if model != "" {
		cfg.Agents.Defaults.SetDefaultModel(model)
	}
	pruneHumanProblems(cfg)

	if problem := providers.CheckDefaultModel(context.Background(), cfg); problem != nil {
		return fmt.Errorf("error creating provider: %w", problem)
	}
	providers.LogBypassEnabled(cfg)

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()
	agentLoop, err := agent.NewAgentLoop(context.Background(), cfg, msgBus)
	if err != nil {
		return fmt.Errorf("error creating agent loop: %w", err)
	}
	defer agentLoop.Close(context.Background())

	// Print agent startup info (only for interactive mode)
	startupInfo := agentLoop.GetStartupInfo()
	// Both sections are always maps; a nil map on mismatch just logs nil fields.
	var toolsInfo, skillsInfo map[string]any
	if v, ok := startupInfo["tools"].(map[string]any); ok {
		toolsInfo = v
	}
	if v, ok := startupInfo["skills"].(map[string]any); ok {
		skillsInfo = v
	}
	logger.InfoCF("agent", "Agent initialized",
		map[string]any{
			"tools_count":      toolsInfo["count"],
			"skills_total":     skillsInfo["total"],
			"skills_available": skillsInfo["available"],
		})

	if message != "" {
		ctx := context.Background()
		response, err := agentLoop.ProcessDirect(ctx, message, cliSessionKey)
		if err != nil {
			return fmt.Errorf("error processing message: %w", err)
		}
		fmt.Printf("\n%s\n", response)
		return nil
	}

	fmt.Print("Interactive mode (Ctrl+C to exit)\n\n")
	interactiveMode(agentLoop)

	return nil
}

func interactiveMode(agentLoop *agent.AgentLoop) {
	prompt := "You: "

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          prompt,
		HistoryFile:     filepath.Join(os.TempDir(), ".claw_history"),
		HistoryLimit:    100,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		logger.ErrorCF("agent", "Error initializing readline, falling back to simple input mode", map[string]any{"error": err.Error()})
		simpleInteractiveMode(agentLoop)
		return
	}
	defer utils.CloseQuietly(rl)

	for {
		line, err := rl.Readline()
		if err != nil {
			if errors.Is(err, readline.ErrInterrupt) || errors.Is(err, io.EOF) {
				fmt.Println("\nGoodbye!")
				return
			}
			logger.ErrorCF("agent", "Error reading input", map[string]any{"error": err.Error()})
			continue
		}

		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}

		if input == "exit" || input == "quit" {
			fmt.Println("Goodbye!")
			return
		}

		ctx := context.Background()
		response, err := agentLoop.ProcessDirect(ctx, input, cliSessionKey)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			continue
		}

		fmt.Printf("\n%s\n\n", response)
	}
}

func simpleInteractiveMode(agentLoop *agent.AgentLoop) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("You: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println("\nGoodbye!")
				return
			}
			logger.ErrorCF("agent", "Error reading input", map[string]any{"error": err.Error()})
			// A failed read fails again on the next call: stop instead of
			// spinning on it.
			return
		}

		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}

		if input == "exit" || input == "quit" {
			fmt.Println("Goodbye!")
			return
		}

		ctx := context.Background()
		response, err := agentLoop.ProcessDirect(ctx, input, cliSessionKey)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			continue
		}

		fmt.Printf("\n%s\n\n", response)
	}
}
