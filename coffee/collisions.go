package coffee

// Static AllowedCollision sets permitted during specific contact phases of the
// brew cycle (e.g. seating the portafilter, pressing the button, carrying a
// held cup near a modeled surface).

var coffeeBrewingCollisions = []AllowedCollision{
	{Frame1: componentFilter, Frame2: "coffee-machine-actuation-area"},
	{Frame1: "portafilter-handle", Frame2: "coffee-machine-actuation-area"},
	{Frame1: componentClaws, Frame2: "coffee-machine-actuation-area"},
	{Frame1: "gripper:claws", Frame2: "coffee-machine-actuation-area"},
}

var filterGrabCollisions = []AllowedCollision{
	{Frame1: componentClaws, Frame2: "portafilter-handle"},
	{Frame1: "gripper:claws", Frame2: "portafilter-handle"},
	{Frame1: "gripper:case-gripper", Frame2: "portafilter-handle"},
}

var cleaningCollisions = []AllowedCollision{
	{Frame1: componentFilter, Frame2: "cleaner-top"},
	{Frame1: "portafilter-handle", Frame2: "cleaner-top"},
	{Frame1: componentClaws, Frame2: "cleaner-top"},
}

var clawCoffeeButtonCollisions = []AllowedCollision{
	{Frame1: componentClaws, Frame2: "coffee-machine-buffer-front"},
	{Frame1: "gripper:claws", Frame2: "coffee-machine-buffer-front"},
}

// Held-item surface collisions (track_held_geometry). When a cup/glass geometry
// is attached to the gripper, the held item must be allowed to approach the
// modeled surfaces it legitimately gets close to during a contact phase — the
// same allowances the bare claws already carry. The gripper-overlap pairs
// (heldItemSelfCollisions) are auto-injected for every held move; these cover
// the per-surface phases and are applied via heldItemSurfaceCollisions so they
// only take effect while an item is actually attached.
var heldItemMachineCollisions = []AllowedCollision{
	{Frame1: heldItemFrameName, Frame2: "coffee-machine-base"},
}

var heldItemServingAreaCollisions = []AllowedCollision{
	{Frame1: heldItemFrameName, Frame2: servingAreaFrameName},
	{Frame1: heldItemFrameName, Frame2: "shelf-top"},
}

// heldItemStagingCollisions allows the held glass to approach the table surfaces
// it legitimately gets close to while being set down in the staging area.
var heldItemStagingCollisions = []AllowedCollision{
	{Frame1: heldItemFrameName, Frame2: "table"},
	{Frame1: heldItemFrameName, Frame2: "table-right"},
}

// doorOpenCollisions permits the gripper to contact the grasp frame (the fridge
// handle ball) while gripping and pulling the door open (openDoor, door.go).
// Built from the configured frame name. If the sweep also trips on the gripper
// nearing the door panel at the handle edge, add {claws, fridge-door}.
func doorOpenCollisions(graspFrame string) []AllowedCollision {
	return []AllowedCollision{
		{Frame1: "gripper:claws", Frame2: graspFrame},
		{Frame1: componentClaws, Frame2: graspFrame},
	}
}
