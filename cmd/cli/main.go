package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot"
	"go.viam.com/rdk/robot/client"
	"go.viam.com/rdk/services/motion"
	"go.viam.com/rdk/spatialmath"
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
	case "move-to-pose":
		return runMoveToPose(os.Args[2:])
	case "get-pose":
		return runGetPose(os.Args[2:])
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", os.Args[1])
	}
}

func printUsage() {
	fmt.Println("Usage: beanjamin-cli <command> [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  move-to-pose  Move an arm to a specified pose via the Motion service")
	fmt.Println("  get-pose      Get the current pose of a component in the world frame")
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

func runMoveToPose(args []string) error {
	flagSet := flag.NewFlagSet("move-to-pose", flag.ExitOnError)
	conn := addConnFlags(flagSet)

	componentName := flagSet.String("component-name", "arm", "Name of the arm to move")

	// Pose flags (position in mm)
	x := flagSet.Float64("x", 0, "X position in mm")
	y := flagSet.Float64("y", 0, "Y position in mm")
	z := flagSet.Float64("z", 0, "Z position in mm")

	// Orientation flags (OrientationVector, degrees)
	ox := flagSet.Float64("ox", 0, "Orientation vector X component")
	oy := flagSet.Float64("oy", 0, "Orientation vector Y component")
	oz := flagSet.Float64("oz", 1, "Orientation vector Z component")
	theta := flagSet.Float64("theta", 0, "Orientation angle in degrees")

	frame := flagSet.String("frame", "world", "Reference frame for the destination pose")

	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if err := conn.validate(); err != nil {
		return err
	}

	ctx := context.Background()
	logger := logging.NewLogger("cli")

	machine, err := conn.connect(ctx, logger)
	if err != nil {
		return err
	}
	defer machine.Close(ctx)

	motionService, err := motion.FromRobot(machine, "builtin")
	if err != nil {
		return fmt.Errorf("getting motion service: %w", err)
	}

	pose := spatialmath.NewPose(
		r3.Vector{X: *x, Y: *y, Z: *z},
		&spatialmath.OrientationVector{OX: *ox, OY: *oy, OZ: *oz, Theta: *theta * math.Pi / 180},
	)
	destination := referenceframe.NewPoseInFrame(*frame, pose)

	// Build WorldState from the robot's frame system so the planner avoids obstacles.
	fsCfg, err := machine.FrameSystemConfig(ctx)
	if err != nil {
		return fmt.Errorf("getting frame system config: %w", err)
	}
	fs, err := referenceframe.NewFrameSystem("robot", fsCfg.Parts, nil)
	if err != nil {
		return fmt.Errorf("building frame system: %w", err)
	}
	inputs, err := machine.CurrentInputs(ctx)
	if err != nil {
		return fmt.Errorf("getting current inputs: %w", err)
	}
	geomMap, err := referenceframe.FrameSystemGeometries(fs, inputs)
	if err != nil {
		return fmt.Errorf("computing frame system geometries: %w", err)
	}
	obstacles := make([]*referenceframe.GeometriesInFrame, 0, len(geomMap))
	for _, g := range geomMap {
		obstacles = append(obstacles, g)
	}
	worldState, err := referenceframe.NewWorldState(obstacles, nil)
	if err != nil {
		return fmt.Errorf("creating world state: %w", err)
	}

	logger.Infof("Moving %q to (%.1f, %.1f, %.1f) in frame %q with %d obstacle frames",
		*componentName, *x, *y, *z, *frame, len(obstacles))
	_, err = motionService.Move(ctx, motion.MoveReq{
		ComponentName: *componentName,
		Destination:   destination,
		WorldState:    worldState,
	})
	if err != nil {
		return fmt.Errorf("motion.Move failed: %w", err)
	}

	logger.Infof("Move completed successfully")
	return nil
}

func runGetPose(args []string) error {
	flagSet := flag.NewFlagSet("get-pose", flag.ExitOnError)
	conn := addConnFlags(flagSet)

	componentName := flagSet.String("component-name", "arm", "Name of the component to query")

	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if err := conn.validate(); err != nil {
		return err
	}

	ctx := context.Background()
	logger := logging.NewLogger("cli")

	machine, err := conn.connect(ctx, logger)
	if err != nil {
		return err
	}
	defer machine.Close(ctx)

	motionService, err := motion.FromRobot(machine, "builtin")
	if err != nil {
		return fmt.Errorf("getting motion service: %w", err)
	}

	poseInFrame, err := motionService.GetPose(ctx, *componentName, "world", nil, nil)
	if err != nil {
		return fmt.Errorf("motion.GetPose failed: %w", err)
	}

	pos := poseInFrame.Pose().Point()
	orient := poseInFrame.Pose().Orientation().OrientationVectorDegrees()
	fmt.Printf("Component: %s\n", *componentName)
	fmt.Printf("Frame:     %s\n", poseInFrame.Parent())
	fmt.Printf("Position:  x=%.2f  y=%.2f  z=%.2f (mm)\n", pos.X, pos.Y, pos.Z)
	fmt.Printf("Orientation: ox=%.4f  oy=%.4f  oz=%.4f  theta=%.2f (deg)\n",
		orient.OX, orient.OY, orient.OZ, orient.Theta)

	return nil
}
