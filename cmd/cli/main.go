// Capability 6 demo: module deployment and update.
//
// Usage:
//
//	go run main.go --host <machine-address>
//	go run main.go --host <machine-address> --cmd <command>
package main

import (
	"beanjamin"
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/erh/vmodutils"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/services/generic"
)

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	ctx := context.Background()
	logger := logging.NewLogger("cli")

	host := flag.String("host", "", "Machine address (required — get from Viam app → Connect tab)")
	cmd := flag.String("cmd", "", "command to execute (move, go, reset, wipe, skill, etc..)")
	flag.Parse()

	if *host == "" {
		return fmt.Errorf("--host is required")
	}

	machine, err := vmodutils.ConnectToHostFromCLIToken(ctx, *host, logger)
	if err != nil {
		return fmt.Errorf("connecting to machine: %w", err)
	}
	defer machine.Close(ctx)

	deps, err := vmodutils.MachineToDependencies(machine)
	if err != nil {
		return err
	}

	cfg := &beanjamin.Config{
		PoseSwitcherName:      "multi-pose-execution-switch",
		ClawsPoseSwitcherName: "claws-position-switch",
		ArmName:               "arm",
		GripperName:           "gripper",
		SpeechServiceName:     "speech",
		VizURL:                "",
		Sequences: map[string][]beanjamin.Step{
			"brew": {
				{PoseName: "grinder_approach", PauseSec: 1},
				{PoseName: "grinder_activate", PauseSec: 1},
				{PoseName: "grinder_approach", PauseSec: 5},
				{PoseName: "tamper_approach", PauseSec: 1},
				{PoseName: "tamper_activate", PauseSec: 2},
				{PoseName: "tamper_approach", PauseSec: 1},
				{PoseName: "coffee_approach", PauseSec: 1},
				{PoseName: "coffee_in", PauseSec: 1},
				{PoseName: "coffee_locked_final", PauseSec: 5},
			},
		},
	}

	_, _, err = cfg.Validate("")
	if err != nil {
		return err
	}

	coffee, err := beanjamin.NewCoffee(ctx, deps, generic.Named("coffee"), cfg, logger)
	if err != nil {
		return fmt.Errorf("getting coffee service: %w", err)
	}

	switch *cmd {
	case "grind_coffee":
		res, err := coffee.DoCommand(ctx, map[string]interface{}{
			"execute_action": "grind_coffee",
		})
		if err != nil {
			return err
		}
		logger.Infof("res: %v", res)
		return nil
	
	default:
		return fmt.Errorf("unknown command [%s]", *cmd)
	}
}
