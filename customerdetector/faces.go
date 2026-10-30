package customerdetector

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxListedFaces caps the photos list_faces returns, so one response stays well
// inside gRPC's message limit (a kiosk capture is ~30 KB, ~40 KB encoded).
const maxListedFaces = 40

// faceImageMIME maps the extensions the face-identification service reads to
// their MIME types.
var faceImageMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
}

// listFaces returns the photos registered under email, with the name on their
// record, so a kiosk can show a customer exactly what the matcher knows them
// by. An email with no photos is not an error: it returns an empty list.
func (cd *customerDetector) listFaces(email string) (map[string]any, error) {
	dir, err := cd.customerDir(email)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read faces for %q: %w", email, err)
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() && faceImageMIME[strings.ToLower(filepath.Ext(e.Name()))] != "" {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	truncated := len(files) > maxListedFaces
	if truncated {
		files = files[len(files)-maxListedFaces:]
	}

	faces := make([]map[string]any, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			cd.logger.Warnf("skipping unreadable face %s for %q: %v", f, email, err)
			continue
		}
		faces = append(faces, map[string]any{
			"file":  f,
			"mime":  faceImageMIME[strings.ToLower(filepath.Ext(f))],
			"image": base64.StdEncoding.EncodeToString(data),
		})
	}

	cd.mu.RLock()
	rec, registered := cd.customers[email]
	cd.mu.RUnlock()
	out := map[string]any{
		"email":      email,
		"registered": registered,
		"faces":      faces,
		"truncated":  truncated,
	}
	if registered {
		out["name"] = rec.Name
	}
	return out, nil
}

// deleteFace removes one photo from email's registered faces and has the vision
// service forget it. file must be the bare name list_faces returned: it is
// joined onto the customer's own directory, so a path that could reach
// anywhere else is refused.
func (cd *customerDetector) deleteFace(ctx context.Context, email, file string) (map[string]any, error) {
	dir, err := cd.customerDir(email)
	if err != nil {
		return nil, err
	}
	if file == "" || file != filepath.Base(file) || file == "." || file == ".." ||
		faceImageMIME[strings.ToLower(filepath.Ext(file))] == "" {
		return nil, fmt.Errorf("file must be the name of a photo, got %q", file)
	}
	path := filepath.Join(dir, file)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no photo %q for %q", file, email)
		}
		return nil, fmt.Errorf("failed to check photo: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a photo file", file)
	}
	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("failed to delete photo: %w", err)
	}
	cd.logger.Infof("deleted face %s for %q", file, email)

	// The matcher keeps embeddings of every photo it has read, so the deleted
	// one keeps matching until they are recomputed.
	vis, err := cd.getVision()
	if err != nil {
		return nil, fmt.Errorf("photo deleted, but the vision service could not forget it yet: %w", err)
	}
	if _, err := vis.DoCommand(ctx, map[string]any{"command": "recompute_embeddings"}); err != nil {
		return nil, fmt.Errorf("photo deleted, but recomputing embeddings failed: %w", err)
	}

	remaining := 0
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.Type().IsRegular() && faceImageMIME[strings.ToLower(filepath.Ext(e.Name()))] != "" {
				remaining++
			}
		}
	}
	return map[string]any{"deleted": file, "email": email, "remaining": remaining}, nil
}

// normalizeFaces brings every registered photo to the standard brightness
// (see normalizeBrightness) and has the vision service rebuild its embeddings.
// Photos saved before brightness correction existed are often dark, and dark
// faces of different people look alike to the matcher.
//
// Each photo is copied to a backup directory beside known_faces before it is
// rewritten, and a photo already at the standard brightness is left alone, so
// running it twice changes nothing the second time.
func (cd *customerDetector) normalizeFaces(ctx context.Context) (map[string]any, error) {
	root := filepath.Join(cd.dataDir, knownFacesDir)
	customers, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", root, err)
	}
	backup := filepath.Join(cd.dataDir, "faces_backup_"+time.Now().UTC().Format("20060102T150405Z"))

	var changed, unchanged, failed int
	for _, c := range customers {
		if !c.IsDir() {
			continue
		}
		dir := filepath.Join(root, c.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			cd.logger.Warnf("skipping %s: %v", c.Name(), err)
			failed++
			continue
		}
		for _, f := range files {
			ext := strings.ToLower(filepath.Ext(f.Name()))
			if !f.Type().IsRegular() || (ext != ".jpg" && ext != ".jpeg") {
				continue
			}
			switch cd.normalizeFace(filepath.Join(dir, f.Name()), filepath.Join(backup, c.Name())) {
			case faceNormalized:
				changed++
			case faceUnchanged:
				unchanged++
			default:
				failed++
			}
		}
	}

	res := map[string]any{"normalized": changed, "unchanged": unchanged, "failed": failed}
	if changed == 0 {
		return res, nil
	}
	res["backup"] = backup
	vis, err := cd.getVision()
	if err != nil {
		return nil, fmt.Errorf("photos rewritten (backup in %s), but the vision service is not available to rebuild embeddings: %w", backup, err)
	}
	if _, err := vis.DoCommand(ctx, map[string]any{"command": "recompute_embeddings"}); err != nil {
		return nil, fmt.Errorf("photos rewritten (backup in %s), but recomputing embeddings failed: %w", backup, err)
	}
	cd.logger.Infof("normalized %d photo(s), %d already fine, %d failed; originals in %s", changed, unchanged, failed, backup)
	return res, nil
}

type faceResult int

const (
	faceFailed faceResult = iota
	faceUnchanged
	faceNormalized
)

// normalizeFace rewrites one photo at the standard brightness, after saving
// the original under backupDir. The new file replaces the old one only once it
// is fully written.
func (cd *customerDetector) normalizeFace(path, backupDir string) faceResult {
	orig, err := os.ReadFile(path)
	if err != nil {
		cd.logger.Warnf("failed to read %s: %v", path, err)
		return faceFailed
	}
	img, _, err := image.Decode(bytes.NewReader(orig))
	if err != nil {
		cd.logger.Warnf("failed to decode %s: %v", path, err)
		return faceFailed
	}
	out, changed := normalizeBrightness(img)
	if !changed {
		return faceUnchanged
	}

	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		cd.logger.Warnf("failed to create backup directory: %v", err)
		return faceFailed
	}
	if err := os.WriteFile(filepath.Join(backupDir, filepath.Base(path)), orig, 0o600); err != nil {
		cd.logger.Warnf("failed to back up %s: %v", path, err)
		return faceFailed
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 90}); err != nil {
		cd.logger.Warnf("failed to encode %s: %v", path, err)
		return faceFailed
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		cd.logger.Warnf("failed to write %s: %v", tmp, err)
		return faceFailed
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		cd.logger.Warnf("failed to replace %s: %v", path, err)
		return faceFailed
	}
	return faceNormalized
}
