package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Subdirectories `viam data export binary` creates under its --destination.
const (
	exportDataDir     = "data"
	exportMetadataDir = "metadata"
)

// exportSubdir holds the raw export tree inside the order directory while the
// files are being flattened out of it, and is removed once they are.
const exportSubdir = ".export"

// Mime types of the payloads an order's tag covers. The plan requests are the
// point of this command; the three camera clips are ~130MB against ~80MB of
// plan JSON, so they are only fetched on request.
const (
	planMimeType  = "application/json"
	videoMimeType = "video/mp4"
)

// planTagPrefixes are the tag= value prefixes savePlanRequestAndResponse writes
// (see coffee/motion.go). They are redundant once the tag value is a field in
// the file name, so they get stripped.
var planTagPrefixes = []string{"step_", "motion_", "planning_"}

// planStampPattern matches the "20060102_150405.000" prefix that
// savePlanRequestAndResponse gives every plan-request file.
var planStampPattern = regexp.MustCompile(`^\d{8}_\d{6}\.\d{3}`)

// orderFile is one exported file and the name it should be written under.
type orderFile struct {
	// rel is the path relative to the export's data/ directory.
	rel string
	// stamp is the timestamp the module wrote into the base name, or "" when
	// the file carries none (it then keeps its name and gets no sequence
	// number — see collectOrderFiles).
	stamp string
	// name is the flattened name, before the sequence prefix.
	name string
}

func runFetchOrder(args []string) error {
	flagSet := flag.NewFlagSet("fetch-order", flag.ExitOnError)
	out := flagSet.String("out", ".", "Parent directory to create the <order-id> directory in")
	from := flagSet.String("from", "", "Reorganize an existing `viam data export` destination instead of downloading")
	viamBin := flagSet.String("viam", "viam", "Path to the viam CLI binary")
	withVideo := flagSet.Bool("with-video", false,
		"Also download the order's camera clips (~130MB of mp4)")
	timeout := flagSet.Int("timeout", 0, "Seconds to allow the export (0 leaves the viam CLI default)")

	if err := flagSet.Parse(args); err != nil {
		return err
	}
	orderID := flagSet.Arg(0)
	if orderID == "" {
		return fmt.Errorf("usage: beanjamin-cli fetch-order [flags] <order-id>")
	}

	orderDir := filepath.Join(*out, orderID)
	orderDirExisted := isDir(orderDir)
	if err := os.MkdirAll(orderDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", orderDir, err)
	}

	// Files are moved out of an export tree we own, and copied out of one the
	// user pointed us at with --from, leaving their tree intact.
	exportDir := *from
	ownExportDir := ""
	if exportDir == "" {
		exportDir = filepath.Join(orderDir, exportSubdir)
		ownExportDir = exportDir
		if err := os.MkdirAll(exportDir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", exportDir, err)
		}
	}
	move := ownExportDir != ""

	// A run that writes nothing — an export that failed, or a tag that matched
	// no data — must not leave empty directories behind in the caller's cwd.
	written := 0
	defer func() {
		if written > 0 {
			return
		}
		if ownExportDir != "" {
			removeIfNoFiles(ownExportDir)
		}
		if !orderDirExisted {
			// Fails harmlessly if the user had put anything in here.
			os.Remove(orderDir) //nolint:errcheck
		}
	}()

	if ownExportDir != "" {
		if err := exportOrder(*viamBin, exportDir, orderID, *withVideo, *timeout); err != nil {
			return err
		}
	}

	dataRoot, err := exportDataRoot(exportDir)
	if err != nil {
		return err
	}
	files, err := collectOrderFiles(dataRoot, orderID)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no data found for order %s in %s", orderID, dataRoot)
	}

	if written, err = writeOrderFiles(dataRoot, orderDir, files, move); err != nil {
		return err
	}
	// Only ever removes the staging tree we created; a --from tree is the
	// user's and stays untouched.
	if move {
		if err := os.RemoveAll(exportDir); err != nil {
			return fmt.Errorf("removing %s: %w", exportDir, err)
		}
	}
	fmt.Printf("wrote %d file(s) to %s\n", written, orderDir)
	return nil
}

// isDir reports whether path is an existing directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// removeIfNoFiles removes dir when its tree holds no files. A tree that holds a
// partial download is kept: the viam CLI skips what it already has on a retry.
func removeIfNoFiles(dir string) {
	hasFile := false
	// Best-effort cleanup: any doubt (including a walk error) keeps the tree.
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil || !d.IsDir() {
			hasFile = true
			return fs.SkipAll
		}
		return nil
	})
	if !hasFile {
		os.RemoveAll(dir) //nolint:errcheck
	}
}

// exportMimeTypes is the mime-type filter for one export: the plan requests
// always, the camera clips only when asked for.
func exportMimeTypes(withVideo bool) []string {
	if withVideo {
		return []string{planMimeType, videoMimeType}
	}
	return []string{planMimeType}
}

// exportOrder shells out to the viam CLI, which owns the credentials and the
// retry/skip-already-downloaded logic, rather than reimplementing either.
func exportOrder(viamBin, destination, orderID string, withVideo bool, timeoutSec int) error {
	args := []string{"data", "export", "binary", "filter", "--destination", destination, "--tags", orderID}
	for _, mimeType := range exportMimeTypes(withVideo) {
		args = append(args, "--mime-types", mimeType)
	}
	if timeoutSec > 0 {
		args = append(args, "--timeout", strconv.Itoa(timeoutSec))
	}
	cmd := exec.Command(viamBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s data export binary filter --tags %s: %w", viamBin, orderID, err)
	}
	return nil
}

// exportDataRoot resolves the directory the exported payloads live under,
// accepting either an export destination or the data/ directory inside one.
func exportDataRoot(exportDir string) (string, error) {
	dataRoot := filepath.Join(exportDir, exportDataDir)
	if info, err := os.Stat(dataRoot); err == nil && info.IsDir() {
		return dataRoot, nil
	}
	info, err := os.Stat(exportDir)
	if err != nil {
		return "", fmt.Errorf("reading export directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", exportDir)
	}
	return exportDir, nil
}

// collectOrderFiles walks an export's data tree and returns its files in the
// order the plans were executed, each carrying the name it should be written
// under. The metadata/ sibling tree is not read: everything the flattened name
// needs is already in the payload's own path.
func collectOrderFiles(dataRoot, orderID string) ([]orderFile, error) {
	var files []orderFile
	err := filepath.WalkDir(dataRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Guard against being handed an export destination whose data/
			// lookup fell through to the destination itself.
			if d.Name() == exportMetadataDir && path != dataRoot {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dataRoot, path)
		if err != nil {
			return err
		}
		// Non-JSON payloads keep their base name: the camera clips carry the
		// order status the video store stamped into theirs, and a motion-tools
		// visualization snapshot's loader keys off its name prefix.
		if !strings.EqualFold(filepath.Ext(rel), ".json") {
			files = append(files, orderFile{rel: rel, name: filepath.Base(rel)})
			return nil
		}
		name, stamp := flattenPlanName(rel, orderID)
		files = append(files, orderFile{rel: rel, stamp: stamp, name: name})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", dataRoot, err)
	}
	// Stamped files first, in execution order; unstamped ones trail behind in a
	// stable order. rel breaks ties so two plans stamped the same millisecond
	// don't reorder between runs.
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if (a.stamp == "") != (b.stamp == "") {
			return a.stamp != ""
		}
		if a.stamp != b.stamp {
			return a.stamp < b.stamp
		}
		return a.rel < b.rel
	})
	return files, nil
}

// flattenPlanName turns a plan-request path relative to the export's data/
// directory into a flat file name: the timestamp the module wrote, then every
// tag= directory value in path order, then anything left in the base name. It
// also returns the timestamp, which is what the files sort on.
func flattenPlanName(rel, orderID string) (name, stamp string) {
	dir, base := filepath.Split(filepath.ToSlash(rel))
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	var fields []string
	if stamp = planStampPattern.FindString(stem); stamp != "" {
		fields = append(fields, stamp)
		stem = strings.TrimPrefix(stem, stamp)
	}
	for segment := range strings.SplitSeq(strings.Trim(dir, "/"), "/") {
		// Only tag= segments carry information. The rest is wherever the
		// machine's capture directory happened to live, which the export
		// preserves verbatim when it isn't the default ~/.viam/capture.
		value, isTag := strings.CutPrefix(segment, "tag=")
		if !isTag || value == orderID {
			continue
		}
		for _, prefix := range planTagPrefixes {
			if trimmed, found := strings.CutPrefix(value, prefix); found {
				value = trimmed
				break
			}
		}
		fields = appendField(fields, value)
	}
	// The base name repeats the motion label that tag=motion_* already
	// contributed, so this only adds anything when the tag dirs were missing.
	fields = appendField(fields, strings.Trim(stem, "_"))
	if len(fields) == 0 {
		return base, ""
	}
	return strings.Join(fields, "-") + ext, stamp
}

// appendField appends value unless it is empty or already a field.
func appendField(fields []string, value string) []string {
	if value == "" {
		return fields
	}
	if slices.Contains(fields, value) {
		return fields
	}
	return append(fields, value)
}

// writeOrderFiles places each file into orderDir under its flattened name,
// prefixed with its 1-based execution index so alphabetical order is execution
// order. Files with no timestamp are not part of that sequence and keep the
// name collectOrderFiles gave them.
func writeOrderFiles(dataRoot, orderDir string, files []orderFile, move bool) (int, error) {
	width := max(len(fmt.Sprint(len(files))), 3)
	taken := make(map[string]bool, len(files))
	seq := 0
	for _, file := range files {
		name := file.name
		if file.stamp != "" {
			seq++
			name = fmt.Sprintf("%0*d-%s", width, seq, name)
		}
		name = uniqueName(taken, name)
		taken[name] = true
		if err := placeFile(filepath.Join(dataRoot, file.rel), filepath.Join(orderDir, name), move); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}

// uniqueName suffixes name until it is not already taken, so two plans stamped
// the same millisecond in the same step don't overwrite each other.
func uniqueName(taken map[string]bool, name string) string {
	if !taken[name] {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, n, ext)
		if !taken[candidate] {
			return candidate
		}
	}
}

// placeFile moves or copies src to dst, falling back to a copy when a move
// crosses a filesystem boundary.
func placeFile(src, dst string, move bool) error {
	if move {
		if err := os.Rename(src, dst); err == nil {
			return nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close() //nolint:errcheck // read-only

	outFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.Copy(outFile, in); err != nil {
		outFile.Close() //nolint:errcheck // the copy error is what matters
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	if err := outFile.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dst, err)
	}
	return nil
}
