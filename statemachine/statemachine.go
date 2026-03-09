package statemachine

import (
	"fmt"
	"slices"
)

// statePoseNames is the ordered list of states in the coffee-making workflow.
// Some pose names appear at multiple indices because the robot physically revisits
// the same position at different points in the sequence (e.g. grinder_approach
// before and after activation). The index, not the pose name alone, defines the
// current state.
//
// State layout:
//
//	0  home
//	1  grinder_approach  (pre-grind)
//	2  grinder_activate
//	3  grinder_approach  (post-grind)
//	4  tamper_approach   (pre-tamp)
//	5  tamper_activate
//	6  tamper_approach   (post-tamp)
//	7  coffee_approach
//	8  coffee_in
//	9  coffee_locked_final
//	10 dump_grounds        (rotation at dump position)
//	11 pre_dump_grounds    (approach to dump; freely connected to approach group)
var statePoseNames = []string{
	"home",                // 0
	"grinder_approach",    // 1
	"grinder_activate",    // 2
	"grinder_approach",    // 3
	"tamper_approach",     // 4
	"tamper_activate",     // 5
	"tamper_approach",     // 6
	"coffee_approach",     // 7
	"coffee_in",           // 8
	"coffee_locked_final", // 9
	"dump_grounds",        // 10
	"pre_dump_grounds",    // 11
}

// validTransitions maps each state index to the state indices it may move to.
//
// Two rules govern all transitions:
//
//  1. Approach / home states (indices 0, 1, 3, 4, 6, 7) form a freely-connected group.
//     From any of them you may jump to any other approach state or home. In addition,
//     each approach state may enter the activate or "in" step of its own section:
//     - grinder_approach (1 or 3) → grinder_activate (2)
//     - tamper_approach  (4 or 6) → tamper_activate  (5)
//     - coffee_approach  (7)      → coffee_in         (8)
//
//  2. Activate and locked states may ONLY move to adjacent states within their own
//     section; they cannot jump to a different section's approach:
//     - grinder_activate (2)     → grinder_approach (post, 3) only
//     - tamper_activate  (5)     → tamper_approach  (post, 6) only
//     - coffee_in        (8)     ↔ coffee_approach (7) or coffee_locked_final (9)
//     - coffee_locked_final (9)  → coffee_in (8) only (must retrace backward via pivot)
//
// To leave the coffee locking section the robot pivots back to coffee_in (8), then
// moves to coffee_approach (7), from which it may freely go anywhere.
var validTransitions = map[int][]int{
	// ── Approach / home states ── freely reachable from each other + own section entry ──
	0: {1, 3, 4, 6, 7, 10, 11}, // home             → any approach, dump_grounds, pre_dump_grounds
	1: {0, 2, 3, 4, 6, 7, 11},  // grinder_approach (pre)  → home, grinder_activate, any approach, pre_dump_grounds
	3: {0, 1, 2, 4, 6, 7, 11},  // grinder_approach (post) → home, grinder_activate, any approach, pre_dump_grounds
	4: {0, 1, 3, 5, 6, 7, 11},  // tamper_approach  (pre)  → home, tamper_activate,  any approach, pre_dump_grounds
	6: {0, 1, 3, 4, 5, 7, 11},  // tamper_approach  (post) → home, tamper_activate,  any approach, pre_dump_grounds
	7: {0, 1, 3, 4, 6, 8, 11},  // coffee_approach         → home, any approach, coffee_in, pre_dump_grounds

	// ── Activate states ── post-activate approach only ──
	2: {3}, // grinder_activate → grinder_approach (post)
	5: {6}, // tamper_activate  → tamper_approach  (post)

	// ── Coffee locked states ── coffee_locked_final retraces to coffee_in via pivot ──
	8: {7, 9}, // coffee_in          → coffee_approach or coffee_locked_final
	9: {8},    // coffee_locked_final → coffee_in only (pivot back, then retrace to approach)

	// ── Dump states ── pre_dump_grounds is in the approach group; dump_grounds is terminal ──
	10: {0, 11},                // dump_grounds    → home or pre_dump_grounds
	11: {0, 1, 3, 4, 6, 7, 10}, // pre_dump_grounds → approach group + dump_grounds
}

// InferIndex returns the state machine index corresponding to poseName,
// or -1 if the pose is not a known state. For poses that appear at multiple
// indices (grinder_approach, tamper_approach) the lowest index is returned.
func InferIndex(poseName string) int {
	for i, name := range statePoseNames {
		if name == poseName {
			return i
		}
	}
	return -1
}

// PoseNameAt returns the pose name at the given state index.
func PoseNameAt(idx int) string {
	return statePoseNames[idx]
}

// isDirectTransition checks if there is a direct transition from state from to state to.
func isDirectTransition(from, to int) bool {
	return slices.Contains(validTransitions[from], to)
}

// inferIndexFrom returns the state index for poseName that is directly
// reachable from fromIdx, or the lowest index if fromIdx < 0.
// Returns -1 if the pose name is unknown.
func inferIndexFrom(poseName string, fromIdx int) int {
	if fromIdx >= 0 {
		for _, next := range validTransitions[fromIdx] {
			if statePoseNames[next] == poseName {
				return next
			}
		}
	}
	return InferIndex(poseName)
}

// ValidatePath iterates over poseNames, skips any where InferIndex returns -1,
// and for each consecutive pair of known-state indices checks isDirectTransition.
// When a pose name appears at multiple indices, the index reachable from the
// previous state is preferred. startIdx seeds the initial context (-1 means none).
// Returns an error if any consecutive pair lacks a direct transition.
func ValidatePath(poseNames []string, startIdx int) error {
	prevIdx := startIdx
	prevName := ""
	if startIdx >= 0 && startIdx < len(statePoseNames) {
		prevName = statePoseNames[startIdx]
	}
	for _, name := range poseNames {
		idx := inferIndexFrom(name, prevIdx)
		if idx < 0 {
			// Not a known state; skip it.
			continue
		}
		if prevIdx >= 0 && !isDirectTransition(prevIdx, idx) {
			return fmt.Errorf("invalid sequence: no direct state machine transition from %q to %q", prevName, name)
		}
		prevIdx = idx
		prevName = name
	}
	return nil
}

// ResolvePath finds the shortest sequence of state transitions from currentIdx
// to any state whose pose name matches targetPose.
// Returns intermediates (pose names to visit before the target, not including
// the current pose or the final pose) and the final pose name.
// Returns an error if currentIdx < 0 (uninitialized) or no path exists.
func ResolvePath(currentIdx int, targetPose string) (intermediates []string, finalPose string, err error) {
	if currentIdx < 0 {
		return nil, "", fmt.Errorf(
			"state machine: current state is uninitialized; " +
				"call {\"get_state\": true} to see valid indices, then {\"set_state_index\": N} to initialize",
		)
	}

	if statePoseNames[currentIdx] == targetPose {
		return nil, targetPose, nil
	}

	type bfsNode struct {
		stateIdx int
		path     []int
	}

	visited := map[int]bool{currentIdx: true}
	queue := []bfsNode{{stateIdx: currentIdx, path: []int{currentIdx}}}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		for _, next := range validTransitions[node.stateIdx] {
			if visited[next] {
				continue
			}
			newPath := make([]int, len(node.path)+1)
			copy(newPath, node.path)
			newPath[len(node.path)] = next

			if statePoseNames[next] == targetPose {
				// newPath = [currentIdx, ..., finalStateIdx]
				// Intermediates are everything between start and end.
				idxIntermediates := newPath[1 : len(newPath)-1]
				names := make([]string, len(idxIntermediates))
				for i, idx := range idxIntermediates {
					names[i] = statePoseNames[idx]
				}
				return names, targetPose, nil
			}

			visited[next] = true
			queue = append(queue, bfsNode{stateIdx: next, path: newPath})
		}
	}

	return nil, "", fmt.Errorf(
		"state machine: no valid path from %q (index %d) to pose %q",
		statePoseNames[currentIdx], currentIdx, targetPose,
	)
}
