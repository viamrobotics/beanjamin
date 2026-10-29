package customerdetector

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// seedFaces writes photos under email's known_faces directory, plus a record
// when name is non-empty.
func seedFaces(t *testing.T, cd *customerDetector, email, name string, files map[string]string) string {
	t.Helper()
	dir, err := cd.customerDir(email)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, content := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if name != "" {
		cd.customers[email] = &customerRecord{Name: name, Email: email, ImageDir: dir}
	}
	return dir
}

func TestListFacesReturnsPhotosAndName(t *testing.T) {
	cd := newTestDetector(t)
	seedFaces(t, cd, "alice@example.com", "Alice", map[string]string{
		"face_2.jpeg": "two",
		"face_1.jpeg": "one",
		"notes.txt":   "not a photo",
	})

	res, err := cd.listFaces("alice@example.com")
	if err != nil {
		t.Fatalf("listFaces: %v", err)
	}
	faces := res["faces"].([]map[string]any)
	if len(faces) != 2 {
		t.Fatalf("got %d faces, want 2 (the .txt is not a photo): %v", len(faces), faces)
	}
	if faces[0]["file"] != "face_1.jpeg" || faces[1]["file"] != "face_2.jpeg" {
		t.Errorf("faces not sorted by name: %v, %v", faces[0]["file"], faces[1]["file"])
	}
	if got, _ := base64.StdEncoding.DecodeString(faces[0]["image"].(string)); string(got) != "one" {
		t.Errorf("image bytes = %q, want %q", got, "one")
	}
	if faces[0]["mime"] != "image/jpeg" {
		t.Errorf("mime = %v, want image/jpeg", faces[0]["mime"])
	}
	if res["name"] != "Alice" || res["registered"] != true {
		t.Errorf("name/registered = %v/%v, want Alice/true", res["name"], res["registered"])
	}
}

func TestListFacesForUnknownEmailIsEmpty(t *testing.T) {
	cd := newTestDetector(t)
	res, err := cd.listFaces("nobody@example.com")
	if err != nil {
		t.Fatalf("listFaces: %v", err)
	}
	if n := len(res["faces"].([]map[string]any)); n != 0 {
		t.Errorf("got %d faces for an unknown email, want 0", n)
	}
	if res["registered"] != false {
		t.Errorf("registered = %v, want false", res["registered"])
	}
	if _, ok := res["name"]; ok {
		t.Errorf("an unregistered email must not report a name")
	}
}

func TestListFacesRejectsBadEmail(t *testing.T) {
	cd := newTestDetector(t)
	for _, email := range []string{"", "../etc", "a/b@example.com", ".."} {
		if _, err := cd.listFaces(email); err == nil {
			t.Errorf("listFaces(%q) succeeded, want an error", email)
		}
	}
}

func TestDeleteFaceRemovesPhotoAndRecomputes(t *testing.T) {
	cd := newTestDetector(t)
	recomputes := withVision(cd)
	dir := seedFaces(t, cd, "alice@example.com", "Alice", map[string]string{
		"face_1.jpeg": "one",
		"face_2.jpeg": "two",
	})

	res, err := cd.deleteFace(context.Background(), "alice@example.com", "face_1.jpeg")
	if err != nil {
		t.Fatalf("deleteFace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "face_1.jpeg")); !os.IsNotExist(err) {
		t.Errorf("photo still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "face_2.jpeg")); err != nil {
		t.Errorf("the other photo was removed: %v", err)
	}
	if *recomputes != 1 {
		t.Errorf("recompute_embeddings ran %d times, want 1", *recomputes)
	}
	if res["remaining"] != 1 {
		t.Errorf("remaining = %v, want 1", res["remaining"])
	}
}

func TestDeleteFaceRefusesPathsOutsideTheCustomersPhotos(t *testing.T) {
	cd := newTestDetector(t)
	withVision(cd)
	seedFaces(t, cd, "alice@example.com", "Alice", map[string]string{"face_1.jpeg": "one"})
	victim := seedFaces(t, cd, "bob@example.com", "Bob", map[string]string{"face_1.jpeg": "bobs"})

	for _, file := range []string{
		"",
		".",
		"..",
		"../bob@example.com/face_1.jpeg",
		"/etc/passwd",
		filepath.Join(victim, "face_1.jpeg"),
		"face_1.txt",
		"customers.json",
	} {
		if _, err := cd.deleteFace(context.Background(), "alice@example.com", file); err == nil {
			t.Errorf("deleteFace(%q) succeeded, want an error", file)
		}
	}
	if _, err := os.Stat(filepath.Join(victim, "face_1.jpeg")); err != nil {
		t.Errorf("another customer's photo was deleted: %v", err)
	}
}

func TestDeleteFaceMissingPhotoIsAnError(t *testing.T) {
	cd := newTestDetector(t)
	withVision(cd)
	seedFaces(t, cd, "alice@example.com", "Alice", map[string]string{"face_1.jpeg": "one"})
	if _, err := cd.deleteFace(context.Background(), "alice@example.com", "face_9.jpeg"); err == nil {
		t.Error("deleting a photo that does not exist succeeded, want an error")
	}
}

func TestDeleteFaceRefusesASymlink(t *testing.T) {
	cd := newTestDetector(t)
	withVision(cd)
	dir := seedFaces(t, cd, "alice@example.com", "Alice", nil)
	target := filepath.Join(t.TempDir(), "secret.jpeg")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "face_1.jpeg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := cd.deleteFace(context.Background(), "alice@example.com", "face_1.jpeg"); err == nil {
		t.Error("deleteFace followed a symlink, want an error")
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the symlink's target was removed: %v", err)
	}
}

func TestListFacesDoesNotLeakAcrossCustomers(t *testing.T) {
	cd := newTestDetector(t)
	seedFaces(t, cd, "alice@example.com", "Alice", map[string]string{"face_1.jpeg": "alices"})
	seedFaces(t, cd, "bob@example.com", "Bob", map[string]string{"face_1.jpeg": "bobs"})
	res, err := cd.listFaces("bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	faces := res["faces"].([]map[string]any)
	if len(faces) != 1 {
		t.Fatalf("got %d faces, want 1", len(faces))
	}
	if got, _ := base64.StdEncoding.DecodeString(faces[0]["image"].(string)); string(got) != "bobs" {
		t.Errorf("bob's list returned %q", got)
	}
}
