package multiposesexecutionswitch

import (
	"strings"
	"testing"

	commonpb "go.viam.com/api/common/v1"
)

func TestValidate(t *testing.T) {
	basePose := &commonpb.Pose{X: 100, Y: 200, Z: 300, OX: 0, OY: 0, OZ: 1, Theta: 45}

	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "missing component_name",
			cfg:     Config{Motion: "builtin", Poses: []PoseConf{{PoseName: "a", PoseValue: basePose}}},
			wantErr: "component_name",
		},
		{
			name:    "missing motion",
			cfg:     Config{ComponentName: "arm", Poses: []PoseConf{{PoseName: "a", PoseValue: basePose}}},
			wantErr: "motion",
		},
		{
			name:    "no poses",
			cfg:     Config{ComponentName: "arm", Motion: "builtin"},
			wantErr: "poses",
		},
		{
			name: "missing pose_name",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{{PoseValue: basePose}},
			},
			wantErr: "pose_name",
		},
		{
			name: "duplicate pose_name",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", PoseValue: basePose},
					{PoseName: "a", PoseValue: basePose},
				},
			},
			wantErr: "duplicate",
		},
		{
			name: "both pose_value and baseline",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", PoseValue: basePose},
					{PoseName: "b", PoseValue: basePose, Baseline: "a"},
				},
			},
			wantErr: "cannot have both",
		},
		{
			name: "neither pose_value nor baseline",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{{PoseName: "a"}},
			},
			wantErr: "must have either",
		},
		{
			name: "translation without baseline",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", PoseValue: basePose, Translation: &Translation{X: 10}},
				},
			},
			wantErr: "require \"baseline\"",
		},
		{
			name: "orientation without baseline",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", PoseValue: basePose, Orientation: &Orientation{OZ: 1, Theta: 90}},
				},
			},
			wantErr: "require \"baseline\"",
		},
		{
			name: "baseline not found",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", Baseline: "nonexistent"},
				},
			},
			wantErr: "not found",
		},
		{
			name: "baseline cycle (self)",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", Baseline: "a"},
				},
			},
			wantErr: `cycle detected involving poses: "a"`,
		},
		{
			name: "baseline cycle (two poses)",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", Baseline: "b"},
					{PoseName: "b", Baseline: "a"},
				},
			},
			wantErr: `cycle detected involving poses: "a", "b"`,
		},
		{
			name: "baseline cycle (three poses)",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", Baseline: "c"},
					{PoseName: "b", Baseline: "a"},
					{PoseName: "c", Baseline: "b"},
				},
			},
			wantErr: `cycle detected involving poses: "a", "b", "c"`,
		},
		{
			name: "valid absolute pose",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{{PoseName: "a", PoseValue: basePose}},
			},
		},
		{
			name: "valid baseline with translation",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", PoseValue: basePose},
					{PoseName: "b", Baseline: "a", Translation: &Translation{X: -100}},
				},
			},
		},
		{
			name: "valid baseline with orientation",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", PoseValue: basePose},
					{PoseName: "b", Baseline: "a", Orientation: &Orientation{OZ: 1, Theta: 90}},
				},
			},
		},
		{
			name: "valid baseline with both translation and orientation",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", PoseValue: basePose},
					{PoseName: "b", Baseline: "a", Translation: &Translation{Z: 100}, Orientation: &Orientation{OY: 1, Theta: 180}},
				},
			},
		},
		{
			name: "valid baseline only (alias)",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", PoseValue: basePose},
					{PoseName: "b", Baseline: "a"},
				},
			},
		},
		{
			name: "valid chained baselines",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", PoseValue: basePose},
					{PoseName: "b", Baseline: "a", Translation: &Translation{X: 10}},
					{PoseName: "c", Baseline: "b", Translation: &Translation{Y: 20}},
				},
			},
		},
		{
			name: "valid forward reference",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", Baseline: "b", Translation: &Translation{X: -100}},
					{PoseName: "b", PoseValue: basePose},
				},
			},
		},
		{
			name: "valid multiple poses referencing same baseline",
			cfg: Config{
				ComponentName: "arm", Motion: "builtin",
				Poses: []PoseConf{
					{PoseName: "a", Baseline: "base", Translation: &Translation{X: 10}},
					{PoseName: "base", PoseValue: basePose},
					{PoseName: "c", Baseline: "base", Translation: &Translation{Z: 50}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.cfg.Validate("test")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if got := err.Error(); !strings.Contains(got, tt.wantErr) {
				t.Fatalf("error %q does not contain %q", got, tt.wantErr)
			}
		})
	}
}

func TestResolvePoses(t *testing.T) {
	basePose := &commonpb.Pose{X: 100, Y: 200, Z: 300, OX: 0, OY: 0, OZ: 1, Theta: 45}

	t.Run("absolute pose", func(t *testing.T) {
		resolved := resolvePoses([]PoseConf{
			{PoseName: "a", PoseValue: basePose},
		})
		assertPoseEqual(t, resolved[0], poseValues{X: 100, Y: 200, Z: 300, OZ: 1, Theta: 45})
	})

	t.Run("baseline with translation", func(t *testing.T) {
		resolved := resolvePoses([]PoseConf{
			{PoseName: "a", PoseValue: basePose},
			{PoseName: "b", Baseline: "a", Translation: &Translation{X: -50, Z: 100}},
		})
		assertPoseEqual(t, resolved[1], poseValues{X: 50, Y: 200, Z: 400, OZ: 1, Theta: 45})
	})

	t.Run("baseline with orientation override", func(t *testing.T) {
		resolved := resolvePoses([]PoseConf{
			{PoseName: "a", PoseValue: basePose},
			{PoseName: "b", Baseline: "a", Orientation: &Orientation{OY: 1, Theta: 90}},
		})
		assertPoseEqual(t, resolved[1], poseValues{X: 100, Y: 200, Z: 300, OY: 1, Theta: 90})
	})

	t.Run("baseline with translation and orientation", func(t *testing.T) {
		resolved := resolvePoses([]PoseConf{
			{PoseName: "a", PoseValue: basePose},
			{PoseName: "c", Baseline: "a", Translation: &Translation{Z: 100}, Orientation: &Orientation{OX: 1, Theta: 180}},
		})
		assertPoseEqual(t, resolved[1], poseValues{X: 100, Y: 200, Z: 400, OX: 1, Theta: 180})
	})

	t.Run("baseline alias (no translation or orientation)", func(t *testing.T) {
		resolved := resolvePoses([]PoseConf{
			{PoseName: "a", PoseValue: basePose},
			{PoseName: "b", Baseline: "a"},
		})
		assertPoseEqual(t, resolved[1], poseValues{X: 100, Y: 200, Z: 300, OZ: 1, Theta: 45})
	})

	t.Run("chained baselines", func(t *testing.T) {
		resolved := resolvePoses([]PoseConf{
			{PoseName: "a", PoseValue: basePose},
			{PoseName: "b", Baseline: "a", Translation: &Translation{X: 10}},
			{PoseName: "c", Baseline: "b", Translation: &Translation{Y: 20}},
		})
		// c = a + {X:10} + {Y:20}
		assertPoseEqual(t, resolved[2], poseValues{X: 110, Y: 220, Z: 300, OZ: 1, Theta: 45})
	})

	t.Run("forward reference", func(t *testing.T) {
		resolved := resolvePoses([]PoseConf{
			{PoseName: "a", Baseline: "b", Translation: &Translation{X: -50}},
			{PoseName: "b", PoseValue: basePose},
		})
		// a = b + {X:-50} = (100-50, 200, 300)
		assertPoseEqual(t, resolved[0], poseValues{X: 50, Y: 200, Z: 300, OZ: 1, Theta: 45})
		assertPoseEqual(t, resolved[1], poseValues{X: 100, Y: 200, Z: 300, OZ: 1, Theta: 45})
	})

	t.Run("multiple poses sharing same baseline", func(t *testing.T) {
		resolved := resolvePoses([]PoseConf{
			{PoseName: "a", Baseline: "base", Translation: &Translation{X: 10}},
			{PoseName: "base", PoseValue: basePose},
			{PoseName: "c", Baseline: "base", Translation: &Translation{Z: 50}},
		})
		assertPoseEqual(t, resolved[0], poseValues{X: 110, Y: 200, Z: 300, OZ: 1, Theta: 45})
		assertPoseEqual(t, resolved[1], poseValues{X: 100, Y: 200, Z: 300, OZ: 1, Theta: 45})
		assertPoseEqual(t, resolved[2], poseValues{X: 100, Y: 200, Z: 350, OZ: 1, Theta: 45})
	})
}

func assertPoseEqual(t *testing.T, got, want poseValues) {
	t.Helper()
	if got != want {
		t.Errorf("pose mismatch:\n  got:  %+v\n  want: %+v", got, want)
	}
}

