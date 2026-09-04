package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFlattenPlanName(t *testing.T) {
	const orderID = "5fb95a4c-83f8-4e66-862d-52cbca842ed"
	cases := []struct {
		name      string
		rel       string
		want      string
		wantStamp string
	}{
		{
			name:      "full tag path",
			rel:       "tag=" + orderID + "/tag=step_locking_portafilter/tag=motion_move/tag=planning_success/20260903_141523.123_move.json",
			want:      "20260903_141523.123-locking_portafilter-move-success.json",
			wantStamp: "20260903_141523.123",
		},
		{
			name:      "plan issued outside a step",
			rel:       "tag=" + orderID + "/tag=motion_carry/tag=planning_failure/20260903_141527.880_carry.json",
			want:      "20260903_141527.880-carry-failure.json",
			wantStamp: "20260903_141527.880",
		},
		{
			// The export only strips a leading ~/.viam/capture; any other
			// capture dir keeps its absolute path in the exported tree.
			name:      "capture dir outside .viam/capture",
			rel:       "home/viam/motion-requests/tag=" + orderID + "/tag=step_grinding/tag=motion_circular/tag=planning_success/20260903_141602.410_circular.json",
			want:      "20260903_141602.410-grinding-circular-success.json",
			wantStamp: "20260903_141602.410",
		},
		{
			name:      "no tag directories",
			rel:       "20260903_141640.005_move.json",
			want:      "20260903_141640.005-move.json",
			wantStamp: "20260903_141640.005",
		},
		{
			name: "no timestamp",
			rel:  "tag=" + orderID + "/whatever.json",
			want: "whatever.json",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, stamp := flattenPlanName(c.rel, orderID)
			if got != c.want {
				t.Errorf("name = %q, want %q", got, c.want)
			}
			if stamp != c.wantStamp {
				t.Errorf("stamp = %q, want %q", stamp, c.wantStamp)
			}
		})
	}
}

// TestFetchOrderEndToEnd is the whole point of the command: an export tree in,
// one flat directory out whose alphabetical order is the execution order.
func TestFetchOrderEndToEnd(t *testing.T) {
	const orderID = "oid-42"
	// Deliberately not in execution order, and one plan per step/motion/outcome
	// combination the module writes.
	rels := []string{
		"tag=" + orderID + "/tag=step_brewing/tag=motion_move/tag=planning_failure/20260903_141640.005_move.json",
		"tag=" + orderID + "/tag=step_locking_portafilter/tag=motion_move/tag=planning_success/20260903_141523.123_move.json",
		"tag=" + orderID + "/tag=step_grinding/tag=motion_circular/tag=planning_success/20260903_141602.410_circular.json",
		"tag=" + orderID + "/tag=step_locking_portafilter/tag=motion_carry/tag=planning_success/20260903_141527.880_carry.json",
	}
	dataRoot := filepath.Join(t.TempDir(), "data")
	for _, rel := range rels {
		path := filepath.Join(dataRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rel), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A metadata sibling must not be picked up.
	metaPath := filepath.Join(filepath.Dir(dataRoot), exportMetadataDir, rels[0]+".json")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := collectOrderFiles(dataRoot, orderID)
	if err != nil {
		t.Fatal(err)
	}
	orderDir := t.TempDir()
	if _, err := writeOrderFiles(dataRoot, orderDir, files, false); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(orderDir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	// os.ReadDir sorts by name, so this asserts alphabetical == execution order.
	want := []string{
		"001-20260903_141523.123-locking_portafilter-move-success.json",
		"002-20260903_141527.880-locking_portafilter-carry-success.json",
		"003-20260903_141602.410-grinding-circular-success.json",
		"004-20260903_141640.005-brewing-move-failure.json",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("order directory contents:\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCollectOrderFilesKeepsNonJSONNames(t *testing.T) {
	dataRoot := t.TempDir()
	// A visualization snapshot is not a plan request: its loader keys off the
	// name's prefix, so it must not be renamed or given a sequence number.
	const snapshot = "visualization_snapshot_20260903_141523_000_cup.pb"
	for _, name := range []string{snapshot, "20260903_141523.123_move.json"} {
		if err := os.WriteFile(filepath.Join(dataRoot, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files, err := collectOrderFiles(dataRoot, "oid")
	if err != nil {
		t.Fatal(err)
	}
	orderDir := t.TempDir()
	if _, err := writeOrderFiles(dataRoot, orderDir, files, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(orderDir, snapshot)); err != nil {
		t.Errorf("snapshot should keep its name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(orderDir, "001-20260903_141523.123-move.json")); err != nil {
		t.Errorf("plan request should be numbered: %v", err)
	}
}

func TestWriteOrderFilesDeduplicates(t *testing.T) {
	dataRoot := t.TempDir()
	// Same millisecond, same step, same motion, same outcome: distinguishable
	// only by the export path, so the flattened names collide.
	rels := []string{"a/20260903_141523.123_move.json", "b/20260903_141523.123_move.json"}
	for _, rel := range rels {
		path := filepath.Join(dataRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rel), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files, err := collectOrderFiles(dataRoot, "oid")
	if err != nil {
		t.Fatal(err)
	}
	orderDir := t.TempDir()
	written, err := writeOrderFiles(dataRoot, orderDir, files, false)
	if err != nil {
		t.Fatal(err)
	}
	if written != 2 {
		t.Fatalf("written = %d, want 2", written)
	}
	entries, err := os.ReadDir(orderDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d files, want 2 (a collision overwrote a plan)", len(entries))
	}
}

// TestRunFetchOrderFailedExportLeavesNothing covers the common failure — a viam CLI
// that isn't logged in — which must not litter the working directory.
func TestRunFetchOrderFailedExportLeavesNothing(t *testing.T) {
	out := t.TempDir()
	if err := runFetchOrder([]string{"--out", out, "--viam", "false", "oid-42"}); err == nil {
		t.Fatal("expected an error from a failing viam CLI")
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed run left %v behind, want nothing", entries)
	}
}

// TestRunFetchOrderFailedExportKeepsPartialDownload is the other half of that
// cleanup: a partial export is worth keeping, because the viam CLI skips what
// it has already downloaded on a retry.
func TestRunFetchOrderFailedExportKeepsPartialDownload(t *testing.T) {
	out := t.TempDir()
	// Stands in for the viam CLI: writes one file into --destination, then fails.
	viamBin := filepath.Join(t.TempDir(), "viam")
	script := `#!/bin/sh
while [ $# -gt 0 ] && [ "$1" != "--destination" ]; do shift; done
[ $# -gt 1 ] || exit 2
mkdir -p "$2/data" && echo '{}' > "$2/data/20260903_141523.123_move.json"
exit 1
`
	if err := os.WriteFile(viamBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := runFetchOrder([]string{"--out", out, "--viam", viamBin, "oid-42"}); err == nil {
		t.Fatal("expected an error from a failing viam CLI")
	}
	partial := filepath.Join(out, "oid-42", exportSubdir, exportDataDir, "20260903_141523.123_move.json")
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("partial download should be kept for the retry: %v", err)
	}
}

// TestRunFetchOrderKeepsPreexistingOrderDir guards the cleanup against deleting a
// directory the user had already put something in.
func TestRunFetchOrderKeepsPreexistingOrderDir(t *testing.T) {
	out := t.TempDir()
	notes := filepath.Join(out, "oid-42", "notes.md")
	if err := os.MkdirAll(filepath.Dir(notes), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notes, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runFetchOrder([]string{"--out", out, "--viam", "false", "oid-42"}); err == nil {
		t.Fatal("expected an error from a failing viam CLI")
	}
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("pre-existing file was removed: %v", err)
	}
}

func TestExportMimeTypes(t *testing.T) {
	// The filter is never empty: an unfiltered export would drag in the order's
	// camera clips by default, which is what --with-video is for.
	if got, want := exportMimeTypes(false), []string{planMimeType}; !slices.Equal(got, want) {
		t.Errorf("exportMimeTypes(false) = %v, want %v", got, want)
	}
	if got, want := exportMimeTypes(true), []string{planMimeType, videoMimeType}; !slices.Equal(got, want) {
		t.Errorf("exportMimeTypes(true) = %v, want %v", got, want)
	}
}

// TestExportOrderPassesMimeTypes pins the flags handed to the viam CLI, since
// the whole download depends on them and nothing else asserts the wiring.
func TestExportOrderPassesMimeTypes(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	viamBin := filepath.Join(dir, "viam")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(viamBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := exportOrder(viamBin, "/dest", "oid-42", true, 60); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(recorded))
	want := []string{
		"data", "export", "binary", "filter",
		"--destination", "/dest", "--tags", "oid-42",
		"--mime-types", planMimeType, "--mime-types", videoMimeType,
		"--timeout", "60",
	}
	if !slices.Equal(got, want) {
		t.Errorf("viam args:\ngot:  %v\nwant: %v", got, want)
	}
}

func TestExportDataRoot(t *testing.T) {
	dest := t.TempDir()
	// An export destination resolves to its data/ subdirectory.
	dataDir := filepath.Join(dest, exportDataDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := exportDataRoot(dest); err != nil || got != dataDir {
		t.Errorf("exportDataRoot(dest) = %q, %v; want %q, nil", got, err, dataDir)
	}
	// A directory without one is used as-is, so --from can point at a data/ dir.
	if got, err := exportDataRoot(dataDir); err != nil || got != dataDir {
		t.Errorf("exportDataRoot(dataDir) = %q, %v; want %q, nil", got, err, dataDir)
	}
	if _, err := exportDataRoot(filepath.Join(dest, "missing")); err == nil {
		t.Error("expected an error for a missing directory")
	}
}
