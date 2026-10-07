// Command agent is the agent service: the close workflow, explainer, verifier and investigator, plus the web UI and JSON API.
//
// Built in CC-703 (workflow), CC-1001 (web app); until then it only checks its configuration.
package main

import (
	"context"
	"log/slog"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
)

func main() {
	cli.Main("agent", []string{
		config.EnvDatabaseURL,
		config.EnvBooksMCPURL,
		config.EnvEvidenceMCPURL,
		config.EnvMCPTokenAgent,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if err := cfg.CheckLLM(); err != nil {
		return err
	}
	log.Info("llm", "provider", cfg.LLMProvider, "fast", cfg.LLMModelFast, "strong", cfg.LLMModelStrong)
	log.Info("not implemented yet", "tickets", "CC-703 (workflow), CC-1001 (web app)", "args", args)
	return nil
}
