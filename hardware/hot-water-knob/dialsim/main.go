// Command dialsim is a dry run of turning the hot water dial: it moves the
// empty claws through the turn the printed lever will need, offset out from
// the lever so nothing is touched. It checks reach, joint limits, and that the
// planner can hold the arc, before anything is printed.
//
// Geometry (world frame): the dial axis is parallel to world +Y and comes out
// of the machine's left wall. The claws grip the lever end-on, like the
// portafilter handle, but the lever is tilted `tilt` degrees out from the wall
// (a cone around the dial axis). Without the tilt the gripper lines up with
// the forearm mid-turn (a wrist singularity), and the planner can't hold the
// arc through it. Opening turns the lever counter-clockwise seen from the left
// of the machine (+ about world +Y), from `closed-deg` below horizontal-front
// to `open-deg` above it.
//
// The machine model still carries the portafilter on the claws, so the moves
// allow the `filter` and `portafilter-handle` frames to overlap the machine:
// TAKE THE PORTAFILTER OUT OF THE CLAWS before running this.
//
// Every move waits for Enter. Pause the coffee service first (`cancel`) so the
// keepalive purge can't start mid-run, and keep a hand on the e-stop.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/gripper"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/client"
	"go.viam.com/rdk/services/motion"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/utils/rpc"
)

// lever describes where the claws grip the lever, and the turn.
type lever struct {
	AxisX, AxisZ float64 // dial axis (parallel to world Y)
	GripRadius   float64 // grip point's distance from the axis, along the lever
	Tilt         float64 // lever tilt out from the wall (deg)
	GripY        float64 // world Y of the grip point on the real lever
	Offset       float64 // dry run: how far further out (+Y) than the real lever to run (mm)
	ClosedDeg    float64 // lever angle above horizontal-front when closed
	OpenDeg      float64 // ... when open
	Backoff      float64 // approach distance back along the tool (mm)
	// Roll is the claws' rotation about the tool axis (the orientation
	// vector's theta). At 0 the fingers close side to side, onto the lever's
	// flat sides; the dimples for the finger pegs face up and down, so the
	// fingers must close vertically. -90 rather than +90 keeps the last joint
	// near the middle of its range through the turn.
	Roll float64
}

// defaultLever is the installed lever. The lean and the closed angle come
// from the arm camera's point cloud: the centreline leans 33 degrees out from
// the wall and sits 2.5 degrees below level when closed. The grip is the
// pose taught by hand with the claws closed on the lever, grip point at
// (819.1, -17.3, 320.0): 63.7 mm along the lever from the dial axis at
// x = 872.5 (32 mm closer to the dial than the printed groove, which assumed
// the pegs sit 54 mm behind the grip point). The axis height is set so that
// pose is exactly the closed one. The robot then grips 8-10 mm further out
// along the lever than that taught pose (`--turn` measured 70.7 and 73.7 mm
// after jogging out from the robot's own slide-on), so slide-on stops at 72.
func defaultLever() lever {
	return lever{
		AxisX: 872.5, AxisZ: 322.8,
		GripRadius: 72, Tilt: 33,
		// The taught pose's Y, moved out along the lever to the 72 mm grip.
		GripY:  -17.3 + (72-63.7)*0.5446,
		Offset: 85,
		// The valve pours fully by about 35 degrees of turn.
		ClosedDeg: -2.5, OpenDeg: 35,
		Backoff: 40,
		Roll:    -90,
	}
}

// pose is the grip-point pose with the lever at deg above horizontal-front,
// backed off along the tool by back mm.
func (l lever) pose(deg, back float64) spatialmath.Pose {
	t := l.Tilt * math.Pi / 180
	// With the lever pointing horizontal-front, the tool points along it,
	// back toward the axis and in toward the wall.
	dir := r3.Vector{X: math.Cos(t), Y: -math.Sin(t)}
	grip := r3.Vector{X: -l.GripRadius * math.Cos(t)} // relative to the axis, at GripY
	rot := spatialmath.NewPoseFromOrientation(&spatialmath.R4AA{Theta: deg * math.Pi / 180, RY: 1})
	rel := spatialmath.Compose(rot, spatialmath.NewPose(
		grip.Sub(dir.Mul(back)),
		&spatialmath.OrientationVectorDegrees{OX: dir.X, OY: dir.Y, OZ: dir.Z, Theta: l.Roll},
	))
	return spatialmath.NewPose(rel.Point().Add(r3.Vector{X: l.AxisX, Y: l.GripY + l.Offset, Z: l.AxisZ}), rel.Orientation())
}

// gripToFlange is how far the arm's flange sits behind the grip point, along
// the tool (gripper 105 mm + grip point 130 mm).
const gripToFlange = 235.0

// flangeZ is the height of the arm's flange for a grip-point pose: the lowest
// point of the gripper, with the wrist just below it.
func flangeZ(p spatialmath.Pose) float64 {
	ov := p.Orientation().OrientationVectorDegrees()
	dir := r3.Vector{X: ov.OX, Y: ov.OY, Z: ov.OZ}.Normalize()
	return p.Point().Sub(dir.Mul(gripToFlange)).Z
}

// minFlangeZ is the lowest the flange may go; the wrist housing hangs below it.
const minFlangeZ = 150.0

// portafilterAllowed lets the modelled (but removed) portafilter overlap the
// machine, the table and the lever: with the claws on the lever, the `filter`
// frame sits on the dial axis, inside the collar.
func portafilterAllowed() []motionplan.CollisionSpecificationAllowedFrameCollisions {
	var allows []motionplan.CollisionSpecificationAllowedFrameCollisions
	for _, held := range []string{"filter", "portafilter-handle"} {
		for _, obstacle := range []string{
			"coffee-machine-top-left-dial", "coffee-machine-top", "coffee-machine-buffer-front",
			"coffee-machine-middle", "coffee-machine-actuation-area", "coffee-machine-base", "table",
			"hot-water-collar", "hot-water-lever",
		} {
			allows = append(allows, motionplan.CollisionSpecificationAllowedFrameCollisions{Frame1: held, Frame2: obstacle})
		}
	}
	return allows
}

// step is one move of the dry run.
type step struct {
	Label  string
	Pose   spatialmath.Pose
	Linear bool
}

func (l lever) steps(stepDeg float64) []step {
	steps := []step{
		{"approach (free move)", l.pose(l.ClosedDeg, l.Backoff), false},
		{"in to the closed lever", l.pose(l.ClosedDeg, 0), true},
	}
	n := int(math.Ceil(math.Abs(l.OpenDeg-l.ClosedDeg) / stepDeg))
	at := func(i int) float64 { return l.ClosedDeg + (l.OpenDeg-l.ClosedDeg)*float64(i)/float64(n) }
	for i := 1; i <= n; i++ {
		steps = append(steps, step{fmt.Sprintf("open %d/%d (%+.0f°)", i, n, at(i)), l.pose(at(i), 0), true})
	}
	for i := n - 1; i >= 0; i-- {
		steps = append(steps, step{fmt.Sprintf("close %d/%d (%+.0f°)", n-i, n, at(i)), l.pose(at(i), 0), true})
	}
	return append(steps, step{"back off", l.pose(l.ClosedDeg, l.Backoff), true})
}

// closedAlong is how far p sits beyond the taught grip along the closed
// lever, toward the dial (negative = further out toward the tip).
func closedAlong(l lever, p r3.Vector) float64 {
	o := l.pose(l.ClosedDeg, 0)
	ov := o.Orientation().OrientationVectorDegrees()
	in := r3.Vector{X: ov.OX, Y: ov.OY, Z: ov.OZ}.Normalize()
	return p.Sub(o.Point()).Dot(in)
}

// turnSteps turns the gripped lever open and back closed, from the closed grip.
func (l lever) turnSteps(stepDeg float64) []step {
	var steps []step
	n := int(math.Ceil(math.Abs(l.OpenDeg-l.ClosedDeg) / stepDeg))
	at := func(i int) float64 { return l.ClosedDeg + (l.OpenDeg-l.ClosedDeg)*float64(i)/float64(n) }
	for i := 1; i <= n; i++ {
		steps = append(steps, step{fmt.Sprintf("open %d/%d (%+.0f°)", i, n, at(i)), l.pose(at(i), 0), true})
	}
	for i := n - 1; i >= 0; i-- {
		steps = append(steps, step{fmt.Sprintf("close %d/%d (%+.0f°)", n-i, n, at(i)), l.pose(at(i), 0), true})
	}
	return steps
}

func constraints(linear bool) *motionplan.Constraints {
	c := &motionplan.Constraints{
		CollisionSpecification: []motionplan.CollisionSpecification{{Allows: portafilterAllowed()}},
	}
	if linear {
		c.LinearConstraint = []motionplan.LinearConstraint{{LineToleranceMm: 1, OrientationToleranceDegs: 2}}
	}
	return c
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	l := defaultLever()
	address := flag.String("address", "", "machine address (required)")
	apiKey := flag.String("api-key", os.Getenv("VIAM_API_KEY"), "API key (or VIAM_API_KEY)")
	apiKeyID := flag.String("api-key-id", os.Getenv("VIAM_API_KEY_ID"), "API key ID (or VIAM_API_KEY_ID)")
	motionName := flag.String("motion", "motion", "motion service name")
	flag.Float64Var(&l.Offset, "offset", l.Offset, "run this far out (+Y, mm) from the real lever; under 80 the claws would hit it")
	flag.Float64Var(&l.Tilt, "tilt", l.Tilt, "lever tilt out from the wall (deg)")
	flag.Float64Var(&l.ClosedDeg, "closed-deg", l.ClosedDeg, "lever angle above horizontal-front when closed")
	flag.Float64Var(&l.OpenDeg, "open-deg", l.OpenDeg, "lever angle above horizontal-front when open")
	stepDeg := flag.Float64("step-deg", 5, "pivot step (deg)")
	noPrompt := flag.Bool("yes", false, "don't wait for Enter between moves")
	teach := flag.Float64("teach", 0, "instead of the dry run, make one move to the real lever's line, this many mm "+
		"beyond the grip spot along it (at least 120, to clear the lever's tip), and stop there for jogging")
	grab := flag.Bool("grab", false, "instead of the dry run, open the gripper, slide the claws onto the lever "+
		"from beyond its tip, and close them on it")
	gripperName := flag.String("gripper", "gripper", "gripper component name")
	turn := flag.Bool("turn", false, "instead of the dry run, with the claws already closed on the lever (anywhere along it), "+
		"turn it open and back closed, leaving the gripper closed")
	jog := flag.Bool("jog", false, "instead of the dry run, nudge the claws from where they are, in straight "+
		"lines along the lever's axes, letting them touch the lever (for teaching the grip)")
	flag.Parse()

	if *address == "" || *apiKey == "" || *apiKeyID == "" {
		return fmt.Errorf("--address, --api-key and --api-key-id are required")
	}

	ctx := context.Background()
	logger := logging.NewLogger("dialsim")
	logger.SetLevel(logging.WARN)
	machine, err := client.New(ctx, *address, logger,
		client.WithDialOptions(rpc.WithEntityCredentials(*apiKeyID,
			rpc.Credentials{Type: rpc.CredentialsTypeAPIKey, Payload: *apiKey})))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer machine.Close(ctx) //nolint:errcheck

	mot, err := motion.FromProvider(machine, *motionName)
	if err != nil {
		return err
	}

	if *jog {
		return jogLoop(ctx, machine, mot, l)
	}
	steps := l.steps(*stepDeg)
	cons := constraints
	if *turn {
		l.Offset = 0
		cur, err := machine.TransformPose(ctx, referenceframe.NewPoseInFrame("grip-point", spatialmath.NewZeroPose()), referenceframe.World, nil)
		if err != nil {
			return err
		}
		// Grip wherever the claws are along the lever: re-derive the grip
		// distance from the current grip point, then check it sits on the
		// lever's line.
		along := closedAlong(l, cur.Pose().Point())
		l.GripRadius -= along
		closed := l.pose(l.ClosedDeg, 0)
		if d := cur.Pose().Point().Distance(closed.Point()); d > 8 {
			return fmt.Errorf("the grip point is %.0f mm off the lever's line (closest grip (%.1f, %.1f, %.1f)); jog onto the lever first",
				d, closed.Point().X, closed.Point().Y, closed.Point().Z)
		}
		fmt.Printf("gripping %.1f mm along the lever from the dial axis\n", l.GripRadius)
		steps = l.turnSteps(*stepDeg)
		cons = func(bool) *motionplan.Constraints { return jogConstraints() }
	}
	var grip gripper.Gripper
	if *grab {
		l.Offset = 0
		if grip, err = gripper.FromProvider(machine, *gripperName); err != nil {
			return err
		}
		steps = []step{
			{"to the lever's line, 150 mm beyond the grip (free move)", l.pose(l.ClosedDeg, 150), false},
			{"slide onto the lever", l.pose(l.ClosedDeg, 0), true},
		}
		cons = func(linear bool) *motionplan.Constraints {
			if linear {
				return jogConstraints()
			}
			return constraints(false)
		}
	}
	if *turn || *grab {
		// checked above
	} else if *teach != 0 {
		if *teach < 120 {
			return fmt.Errorf("--teach %.0f would put the claws on the lever; use at least 120", *teach)
		}
		l.Offset = 0
		steps = []step{{fmt.Sprintf("to the lever's line, %.0f mm beyond the grip spot (free move)", *teach), l.pose(l.ClosedDeg, *teach), false}}
	} else if l.Offset < 80 {
		return fmt.Errorf("--offset %.0f would put the claws on the lever; this dry run must stay clear of it", l.Offset)
	}
	fmt.Printf("dial axis at x=%.1f z=%.1f; lever tilted %.0f°, running %.0f mm out from it, turning %+.1f° -> %+.1f°\n",
		l.AxisX, l.AxisZ, l.Tilt, l.Offset, l.ClosedDeg, l.OpenDeg)
	stdin := bufio.NewReader(os.Stdin)
	for i, s := range steps {
		if *grab && i == 1 {
			fmt.Print("\nopen the gripper: Enter to open, Ctrl-C to stop: ")
			if _, err := stdin.ReadString('\n'); err != nil {
				return err
			}
			if err := grip.Open(ctx, nil); err != nil {
				return fmt.Errorf("open gripper: %w", err)
			}
		}
		ov := s.Pose.Orientation().OrientationVectorDegrees()
		pt := s.Pose.Point()
		fz := flangeZ(s.Pose)
		fmt.Printf("\n%s: grip-point at (%.1f, %.1f, %.1f) o=(%.2f, %.2f, %.2f) theta=%.1f, arm flange %.0f mm above the table\n",
			s.Label, pt.X, pt.Y, pt.Z, ov.OX, ov.OY, ov.OZ, ov.Theta, fz)
		if fz < minFlangeZ {
			return fmt.Errorf("%s: arm flange would be %.0f mm above the table (minimum %.0f); raise --closed-deg", s.Label, fz, minFlangeZ)
		}
		if !*noPrompt {
			fmt.Print("  Enter to move, Ctrl-C to stop: ")
			if _, err := stdin.ReadString('\n'); err != nil {
				return err
			}
		}
		start := time.Now()
		if _, err := mot.Move(ctx, motion.MoveReq{
			ComponentName: "grip-point",
			Destination:   referenceframe.NewPoseInFrame(referenceframe.World, s.Pose),
			Constraints:   cons(s.Linear),
			Extra:         map[string]any{"timeout": 15.0},
		}); err != nil {
			return fmt.Errorf("%s: %w", s.Label, err)
		}
		fmt.Printf("  done in %.1fs\n", time.Since(start).Seconds())
	}
	if *grab {
		fmt.Print("\nclose the gripper on the lever: Enter to close, Ctrl-C to stop: ")
		if _, err := stdin.ReadString('\n'); err != nil {
			return err
		}
		if _, err := grip.Grab(ctx, nil); err != nil {
			return fmt.Errorf("close gripper: %w", err)
		}
		fmt.Println("done. Check both pegs are in the dimples, then run --turn.")
		return nil
	}
	if *turn {
		fmt.Println("\ndone. The claws are still closed on the lever: open the gripper, then jog out with --jog (o).")
		return nil
	}
	if *teach != 0 {
		fmt.Println("\ndone. Open the gripper, then jog along the tool onto the lever.")
		return nil
	}
	fmt.Println("\ndone. Send the filter pose switch to `home`, put the portafilter back, then `proceed` on the coffee service.")
	return nil
}

// jogAllowed is constraints(true) plus the claws touching the lever and collar.
func jogConstraints() *motionplan.Constraints {
	c := constraints(true)
	for _, claws := range []string{"coffee-claws-middle", "gripper:claws"} {
		for _, obstacle := range []string{"hot-water-lever", "hot-water-collar"} {
			c.CollisionSpecification[0].Allows = append(c.CollisionSpecification[0].Allows,
				motionplan.CollisionSpecificationAllowedFrameCollisions{Frame1: claws, Frame2: obstacle})
		}
	}
	return c
}

// jogLoop nudges the grip point in straight lines, keeping its orientation.
// Directions follow the lever (closed): in/out along it, toward/away from the
// machine's wall across it, and up/down.
func jogLoop(ctx context.Context, machine *client.RobotClient, mot motion.Service, l lever) error {
	t := l.Tilt * math.Pi / 180
	in := r3.Vector{X: math.Cos(t), Y: -math.Sin(t)} // along the lever, toward the dial
	wall := r3.Vector{X: -math.Sin(t), Y: -math.Cos(t)}
	up := r3.Vector{Z: 1}
	dirs := map[string]r3.Vector{"i": in, "o": in.Mul(-1), "w": wall, "a": wall.Mul(-1), "u": up, "d": up.Mul(-1)}
	fmt.Println(`jog the claws: a letter, optionally a distance in mm (default 5, max 30), then Enter.
  i / o   in toward the dial / out, along the lever
  w / a   toward the machine's wall / away from it, across the lever
  u / d   up / down
  p       print the grip-point pose
  q       quit (the arm stays where it is)
The claws may touch the lever and the collar in these moves: watch them.`)
	stdin := bufio.NewReader(os.Stdin)
	for {
		cur, err := machine.TransformPose(ctx, referenceframe.NewPoseInFrame("grip-point", spatialmath.NewZeroPose()), referenceframe.World, nil)
		if err != nil {
			return err
		}
		pt := cur.Pose().Point()
		fmt.Printf("grip-point (%.1f, %.1f, %.1f) > ", pt.X, pt.Y, pt.Z)
		line, err := stdin.ReadString('\n')
		if err != nil {
			return err
		}
		var cmd string
		mm := 5.0
		if n, _ := fmt.Sscanf(line, "%s %f", &cmd, &mm); n == 0 {
			continue
		}
		switch cmd {
		case "q":
			return nil
		case "p":
			ov := cur.Pose().Orientation().OrientationVectorDegrees()
			fmt.Printf("  x %.1f y %.1f z %.1f  o (%.4f, %.4f, %.4f) theta %.1f\n", pt.X, pt.Y, pt.Z, ov.OX, ov.OY, ov.OZ, ov.Theta)
			continue
		}
		dir, ok := dirs[cmd]
		if !ok {
			fmt.Println("  unknown command")
			continue
		}
		if mm <= 0 || mm > 30 {
			fmt.Println("  distance must be between 0 and 30 mm")
			continue
		}
		goal := spatialmath.NewPose(pt.Add(dir.Mul(mm)), cur.Pose().Orientation())
		if _, err := mot.Move(ctx, motion.MoveReq{
			ComponentName: "grip-point",
			Destination:   referenceframe.NewPoseInFrame(referenceframe.World, goal),
			Constraints:   jogConstraints(),
			Extra:         map[string]any{"timeout": 15.0},
		}); err != nil {
			fmt.Printf("  move failed: %v\n", err)
		}
	}
}
