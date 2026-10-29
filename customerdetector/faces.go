package customerdetector

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
