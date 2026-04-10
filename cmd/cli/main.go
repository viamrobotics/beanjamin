package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/robot"
	"go.viam.com/rdk/robot/client"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/utils/rpc"
)

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	if len(os.Args) < 2 {
		printUsage()
		return fmt.Errorf("no command specified")
	}

	switch os.Args[1] {
	case "say":
		return runSay(os.Args[2:])
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", os.Args[1])
	}
}

func printUsage() {
	fmt.Println("Usage: beanjamin-cli <command> [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  say         Say text aloud via the speech service")
}

// connFlags holds the shared connection flags used by all commands.
type connFlags struct {
	address  *string
	apiKey   *string
	apiKeyID *string
}

func addConnFlags(flagSet *flag.FlagSet) connFlags {
	return connFlags{
		address:  flagSet.String("address", "", "Machine gRPC address (required)"),
		apiKey:   flagSet.String("api-key", os.Getenv("VIAM_API_KEY"), "API key (or set VIAM_API_KEY env var)"),
		apiKeyID: flagSet.String("api-key-id", os.Getenv("VIAM_API_KEY_ID"), "API key ID (or set VIAM_API_KEY_ID env var)"),
	}
}

func (c connFlags) validate() error {
	if *c.address == "" {
		return fmt.Errorf("--address is required")
	}
	if *c.apiKey == "" || *c.apiKeyID == "" {
		return fmt.Errorf("--api-key and --api-key-id are required (or set VIAM_API_KEY / VIAM_API_KEY_ID)")
	}
	return nil
}

func runSay(args []string) error {
	flagSet := flag.NewFlagSet("say", flag.ExitOnError)
	conn := addConnFlags(flagSet)

	serviceName := flagSet.String("service", "speech-1", "Name of the speech service")

	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if err := conn.validate(); err != nil {
		return err
	}

	text := flagSet.Arg(0)
	if text == "" {
		return fmt.Errorf("usage: beanjamin-cli say [flags] \"text to speak\"")
	}

	ctx := context.Background()
	logger := logging.NewLogger("cli")

	machine, err := conn.connect(ctx, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := machine.Close(ctx); err != nil {
			logger.Warnf("closing machine: %v", err)
		}
	}()

	speechSvc, err := generic.FromProvider(machine, *serviceName)
	if err != nil {
		return fmt.Errorf("getting speech service %q: %w", *serviceName, err)
	}

	resp, err := speechSvc.DoCommand(ctx, map[string]interface{}{
		"say": text,
	})
	if err != nil {
		return fmt.Errorf("say failed: %w", err)
	}

	if t, ok := resp["text"].(string); ok && t != "" {
		fmt.Println(t)
	}
	return nil
}

func (c connFlags) connect(ctx context.Context, logger logging.Logger) (robot.Robot, error) {
	machine, err := client.New(ctx, *c.address, logger,
		client.WithDialOptions(rpc.WithEntityCredentials(
			*c.apiKeyID,
			rpc.Credentials{Type: rpc.CredentialsTypeAPIKey, Payload: *c.apiKey},
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to machine: %w", err)
	}
	return machine, nil
}
