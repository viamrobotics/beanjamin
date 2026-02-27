package beanjamin

import (
	"errors"
	"fmt"
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
//	9  coffee_locked_mid
//	10 coffee_locked_final
//	11 dump_grounds       (physically co-located with home, accessible from home only)
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
	"coffee_locked_mid",   // 9
	"coffee_locked_final", // 10
	"dump_grounds",        // 11
}

// validTransitions maps each state index to the state indices it may move to.
//
// Two rules govern all transitions:
//
//  1. Approach / home states (indices 0, 1, 3, 4, 6, 7) form a freely-connected group.
//     From any of them you may jump to any other approach state or home. In addition,
//     each approach state may enter the activate or "in" step of its own section:
//       - grinder_approach (1 or 3) → grinder_activate (2)
//       - tamper_approach  (4 or 6) → tamper_activate  (5)
//       - coffee_approach  (7)      → coffee_in         (8)
//
//  2. Activate and locked states may ONLY move to adjacent states within their own
//     section; they cannot jump to a different section's approach:
//       - grinder_activate (2)     ↔ grinder_approach (1, 3)
//       - tamper_activate  (5)     ↔ tamper_approach  (4, 6)
//       - coffee_in        (8)     ↔ coffee_approach (7) or coffee_locked_mid (9)
//       - coffee_locked_mid (9)    ↔ coffee_in (8) or coffee_locked_final (10)
//       - coffee_locked_final (10) → coffee_locked_mid (9) only (no forward state)
//
// To leave the coffee locking section the robot must retrace step-by-step back to
// coffee_approach (7), from which it may freely go anywhere.
var validTransitions = map[int][]int{
	// ── Approach / home states ── freely reachable from each other + own section entry ──
	0:  {1, 3, 4, 6, 7, 11}, // home             → any approach, dump_grounds
	1:  {0, 2, 3, 4, 6, 7},  // grinder_approach (pre)  → home, grinder_activate, any approach
	3:  {0, 1, 2, 4, 6, 7},  // grinder_approach (post) → home, grinder_activate, any approach
	4:  {0, 1, 3, 5, 6, 7},  // tamper_approach  (pre)  → home, tamper_activate,  any approach
	6:  {0, 1, 3, 4, 5, 7},  // tamper_approach  (post) → home, tamper_activate,  any approach
	7:  {0, 1, 3, 4, 6, 8},  // coffee_approach         → home, any approach, coffee_in

	// ── Activate states ── own section's approach states only ──
	2: {1, 3}, // grinder_activate → grinder_approach (pre or post)
	5: {4, 6}, // tamper_activate  → tamper_approach  (pre or post)

	// ── Coffee locked states ── adjacent within section only ──
	8:  {7, 9},  // coffee_in          → coffee_approach or coffee_locked_mid
	9:  {8, 10}, // coffee_locked_mid  → coffee_in or coffee_locked_final
	10: {9},     // coffee_locked_final → coffee_locked_mid only (must retrace backward)

	// ── Utility states ── only reachable from / return to home ──
	11: {0}, // dump_grounds → home only
}

// inferStateIndex returns the state machine index corresponding to poseName,
// or -1 if the pose is not a known state. For poses that appear at multiple
// indices (grinder_approach, tamper_approach) the lowest index is returned.
func inferStateIndex(poseName string) int {
	for i, name := range statePoseNames {
		if name == poseName {
			return i
		}
	}
	return -1
}

// resolvePath finds the shortest sequence of state transitions from the current
// state to any state whose pose name matches targetPose. It returns:
//   - intermediates: state indices to visit before the target (empty if adjacent)
//   - finalStateIdx: the destination state index
//   - err: non-nil if the state machine is uninitialized or no path exists
//
// The caller should move the robot to each intermediate pose (without constraints
// or pauses) before executing the target step with its full configuration.
func (s *beanjaminCoffee) resolvePath(targetPose string) (intermediates []int, finalStateIdx int, err error) {
	s.mu.Lock()
	current := s.currentStateIdx
	s.mu.Unlock()

	if current < 0 {
		return nil, -1, errors.New(
			"state machine: current state is uninitialized; " +
				"call {\"get_state\": true} to see valid indices, then {\"set_state_index\": N} to initialize",
		)
	}

	type bfsNode struct {
		stateIdx int
		path     []int
	}

	visited := map[int]bool{current: true}
	queue := []bfsNode{{stateIdx: current, path: []int{current}}}

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
				// newPath = [current, ..., finalStateIdx]
				// Intermediates are everything between start and end.
				return newPath[1 : len(newPath)-1], next, nil
			}

			visited[next] = true
			queue = append(queue, bfsNode{stateIdx: next, path: newPath})
		}
	}

	return nil, -1, fmt.Errorf(
		"state machine: no valid path from %q (index %d) to pose %q",
		statePoseNames[current], current, targetPose,
	)
}

// commitTransition records the new state index after a move completes successfully.
func (s *beanjaminCoffee) commitTransition(newStateIdx int) {
	s.mu.Lock()
	s.currentStateIdx = newStateIdx
	s.mu.Unlock()
}

// getState returns the current state machine status for the get_state DoCommand.
func (s *beanjaminCoffee) getState() map[string]interface{} {
	s.mu.Lock()
	idx := s.currentStateIdx
	s.mu.Unlock()

	stateName := "uninitialized"
	if idx >= 0 && idx < len(statePoseNames) {
		stateName = statePoseNames[idx]
	}

	var allowedNext []map[string]interface{}
	if idx >= 0 {
		for _, nextIdx := range validTransitions[idx] {
			allowedNext = append(allowedNext, map[string]interface{}{
				"index": nextIdx,
				"name":  statePoseNames[nextIdx],
			})
		}
	}

	return map[string]interface{}{
		"state_index":         idx,
		"state_name":          stateName,
		"allowed_transitions": allowedNext,
	}
}

// setStateIndex handles the set_state_index DoCommand.
func (s *beanjaminCoffee) setStateIndex(idxRaw interface{}) (map[string]interface{}, error) {
	var idx int
	switch v := idxRaw.(type) {
	case float64:
		idx = int(v)
	case int:
		idx = v
	default:
		return nil, fmt.Errorf("set_state_index: value must be an integer, got %T", idxRaw)
	}
	if idx < 0 || idx >= len(statePoseNames) {
		return nil, fmt.Errorf("set_state_index: %d is out of range [0, %d]", idx, len(statePoseNames)-1)
	}
	s.mu.Lock()
	s.currentStateIdx = idx
	s.mu.Unlock()
	s.logger.Infof("state machine: state manually set to index %d (%q)", idx, statePoseNames[idx])
	return map[string]interface{}{
		"status":      "ok",
		"state_index": idx,
		"state_name":  statePoseNames[idx],
	}, nil
}
