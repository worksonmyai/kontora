package daemon

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
)

func validateSupervisedAgent(agent config.Agent) error {
	if !agent.IsPi() {
		return nil
	}
	args := agent.Args
	if filepath.Base(agent.Binary) != "pi" {
		for i, arg := range args {
			if arg == "--" && i+1 < len(args) {
				args = args[i+2:]
				break
			}
		}
	}
	for i, arg := range args {
		mode, modeFlag := strings.CutPrefix(arg, "--mode=")
		if arg == "--mode" && i+1 < len(args) {
			mode, modeFlag = args[i+1], true
		}
		if arg == "-p" || arg == "--print" || modeFlag && (mode == "json" || mode == "rpc") {
			return fmt.Errorf("supervised Pi requires TUI mode to cancel queued continuation after its final answer; remove --print/-p and --mode json/rpc from this agent's arguments")
		}
	}
	return nil
}
