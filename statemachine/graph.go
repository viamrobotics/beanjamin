package statemachine

import (
	"fmt"
)

// isDirectTransition checks if there is a direct transition from state from to state to.
func isDirectTransition(transitions map[string][]string, from, to string) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// validatePath checks that every pose in poseNames is a known state machine state
// and that each consecutive pair has a direct transition.
// startPose seeds the initial context ("" means none).
func validatePath(transitions map[string][]string, poseNames []string, startPose string) error {
	prevPose := startPose
	for _, name := range poseNames {
		if _, ok := transitions[name]; !ok {
			return fmt.Errorf("unknown state machine pose %q", name)
		}
		if prevPose != "" && !isDirectTransition(transitions, prevPose, name) {
			return fmt.Errorf("invalid sequence: no direct state machine transition from %q to %q", prevPose, name)
		}
		prevPose = name
	}
	return nil
}

// resolvePath finds the shortest sequence of state transitions from currentPose
// to targetPose using BFS on string names.
// Returns intermediates (pose names to visit before the target, not including
// the current pose or the final pose) and the final pose name.
// Returns an error if currentPose == "" (uninitialized), targetPose is not a known
// state, or no path exists.
func resolvePath(transitions map[string][]string, currentPose, targetPose string) (intermediates []string, finalPose string, err error) {
	if currentPose == "" {
		return nil, "", fmt.Errorf(
			"state machine: current state is uninitialized; " +
				"call {\"get_state\": true} to see valid states, then {\"set_state\": \"<name>\"} to initialize",
		)
	}

	if _, ok := transitions[targetPose]; !ok {
		return nil, "", fmt.Errorf("state machine: %q is not a known state", targetPose)
	}

	if currentPose == targetPose {
		return nil, targetPose, nil
	}

	type bfsNode struct {
		pose string
		path []string
	}

	visited := map[string]bool{currentPose: true}
	queue := []bfsNode{{pose: currentPose, path: []string{currentPose}}}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		for _, next := range transitions[node.pose] {
			if visited[next] {
				continue
			}
			newPath := make([]string, len(node.path)+1)
			copy(newPath, node.path)
			newPath[len(node.path)] = next

			if next == targetPose {
				// newPath = [currentPose, ..., targetPose]
				// Intermediates are everything between start and end.
				return newPath[1 : len(newPath)-1], targetPose, nil
			}

			visited[next] = true
			queue = append(queue, bfsNode{pose: next, path: newPath})
		}
	}

	return nil, "", fmt.Errorf(
		"state machine: no valid path from %q to %q",
		currentPose, targetPose,
	)
}
