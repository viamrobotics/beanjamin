package customerdetector

import (
	"context"
	"image"
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
)

func newTestDetector(t *testing.T) *customerDetector {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, knownFacesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return &customerDetector{
		logger:       logging.NewTestLogger(t),
		dataDir:      dataDir,
		customers:    make(map[string]*customerRecord),
		pendingNames: make(map[string]string),
	}
}

func TestValidateEmail(t *testing.T) {
	valid := []string{
		"alice@example.com",
		"first.last+coffee@example.co.uk",
	}
	for _, email := range valid {
		if err := validateEmail(email); err != nil {
			t.Errorf("validateEmail(%q) = %v, want nil", email, err)
		}
	}

	invalid := []string{
		"",
		".",
		"..",
		"../../etc",
		"a@b.com/../../x",
		`a@b.com\..\x`,
		"alice",
		"Alice <alice@example.com>",
		" alice@example.com",
		string(make([]byte, maxEmailLen+1)),
	}
	for _, email := range invalid {
		if err := validateEmail(email); err == nil {
			t.Errorf("validateEmail(%q) = nil, want an error", email)
		}
	}
}

func TestRegisterCustomerRejectsTraversalBeforeCapture(t *testing.T) {
	cd := newTestDetector(t)

	// cd.camera is nil: an email that got past validation would panic on
	// capture, so an error here proves the check runs first.
	for _, email := range []string{"..", ".", "../outside", "a@b.com/../../x"} {
		if _, err := cd.registerCustomer(context.Background(), "Mallory", email, -1); err == nil {
			t.Fatalf("registerCustomer(%q) succeeded, want an error", email)
		}
	}

	if _, err := os.Stat(filepath.Join(cd.dataDir, "outside")); !os.IsNotExist(err) {
		t.Fatalf("traversal email created a directory outside known_faces: %v", err)
	}
	if len(cd.customers) != 0 {
		t.Fatalf("rejected registrations stored %d records", len(cd.customers))
	}
}

func TestRemoveCustomerRefusesImageDirOutsideKnownFaces(t *testing.T) {
	cd := newTestDetector(t)
	sentinel := filepath.Join(cd.dataDir, "customers.json")
	if err := os.WriteFile(sentinel, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A record written before validation existed, whose email ".." resolved
	// to data_dir itself.
	cd.customers[".."] = &customerRecord{Name: "Mallory", Email: "..", ImageDir: cd.dataDir}

	if _, err := cd.removeCustomer(context.Background(), ".."); err == nil {
		t.Fatal("removeCustomer succeeded on an ImageDir outside known_faces, want an error")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("data_dir contents were deleted: %v", err)
	}
	if _, ok := cd.customers[".."]; !ok {
		t.Fatal("refused removal still dropped the record")
	}
}

func TestRemoveCustomerDeletesOwnImageDir(t *testing.T) {
	cd := newTestDetector(t)
	dir, err := cd.customerDir("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "face_1.jpeg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cd.customers["alice@example.com"] = &customerRecord{Name: "Alice", Email: "alice@example.com", ImageDir: dir}

	if _, err := cd.removeCustomer(context.Background(), "alice@example.com"); err != nil {
		t.Fatalf("removeCustomer: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("customer image directory still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cd.dataDir, knownFacesDir)); err != nil {
		t.Fatalf("known_faces itself was removed: %v", err)
	}
}

// stageTestFace stages a blank face the way registerCustomer does after its
// camera capture.
func stageTestFace(t *testing.T, cd *customerDetector, name, email string, pose int) string {
	t.Helper()
	path, err := cd.stageFace(email, pose, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	if err != nil {
		t.Fatalf("stageFace: %v", err)
	}
	cd.pendingNames[email] = name
	return path
}

// withVision installs a vision service that counts recompute_embeddings calls.
func withVision(cd *customerDetector) *int {
	recomputes := 0
	vis := inject.NewVisionService("face-id")
	vis.DoCommandFunc = func(context.Context, map[string]any) (map[string]any, error) {
		recomputes++
		return map[string]any{}, nil
	}
	cd.vision = vis
	return &recomputes
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestStagedFacesStayOutOfKnownFaces(t *testing.T) {
	cd := newTestDetector(t)
	path := stageTestFace(t, cd, "Alice", "alice@example.com", 0)

	rel, err := filepath.Rel(filepath.Join(cd.dataDir, knownFacesDir), path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsLocal(rel) {
		t.Fatalf("staged face %s is inside known_faces, where it would be embedded", path)
	}
	if len(cd.customers) != 0 {
		t.Fatal("staging a face saved a customer record")
	}
}

func TestRetakeReplacesItsPose(t *testing.T) {
	cd := newTestDetector(t)
	for _, pose := range []int{0, 1, 2, 1} {
		stageTestFace(t, cd, "Alice", "alice@example.com", pose)
	}
	pending, _ := cd.pendingDir("alice@example.com")
	if got := countFiles(t, pending); got != 3 {
		t.Fatalf("staged %d faces after retaking pose 1, want 3", got)
	}
}

func TestCancelRegistrationDiscardsStagedFaces(t *testing.T) {
	cd := newTestDetector(t)
	recomputes := withVision(cd)
	for pose := range 3 {
		stageTestFace(t, cd, "Alice", "alice@example.com", pose)
	}

	res, err := cd.cancelRegistration("alice@example.com")
	if err != nil {
		t.Fatalf("cancelRegistration: %v", err)
	}
	if res["discarded"] != 3 {
		t.Fatalf("discarded = %v, want 3", res["discarded"])
	}
	pending, _ := cd.pendingDir("alice@example.com")
	if got := countFiles(t, pending); got != 0 {
		t.Fatalf("%d staged faces survived cancel", got)
	}
	if _, err := cd.finishRegistration(context.Background(), "alice@example.com"); err == nil {
		t.Fatal("finishRegistration after cancel succeeded, want an error")
	}
	if *recomputes != 0 {
		t.Fatalf("recompute_embeddings ran %d times for a cancelled registration", *recomputes)
	}

	if _, err := cd.cancelRegistration("alice@example.com"); err != nil {
		t.Fatalf("second cancelRegistration: %v", err)
	}
}

func TestCancelRegistrationKeepsCommittedFaces(t *testing.T) {
	cd := newTestDetector(t)
	withVision(cd)
	stageTestFace(t, cd, "Alice", "alice@example.com", 0)
	if _, err := cd.finishRegistration(context.Background(), "alice@example.com"); err != nil {
		t.Fatal(err)
	}

	stageTestFace(t, cd, "Alice", "alice@example.com", 0)
	if _, err := cd.cancelRegistration("alice@example.com"); err != nil {
		t.Fatal(err)
	}

	dir, _ := cd.customerDir("alice@example.com")
	if got := countFiles(t, dir); got != 1 {
		t.Fatalf("committed faces = %d after cancelling a re-registration, want 1", got)
	}
	if _, ok := cd.customers["alice@example.com"]; !ok {
		t.Fatal("cancelling a re-registration dropped the existing record")
	}
}

func TestFinishRegistrationCommitsFacesAndKeepsHistory(t *testing.T) {
	cd := newTestDetector(t)
	recomputes := withVision(cd)
	dir, _ := cd.customerDir("alice@example.com")
	cd.customers["alice@example.com"] = &customerRecord{
		Name: "Alice", Email: "alice@example.com", ImageDir: dir,
		Orders: []orderHistoryEntry{{Drink: "espresso"}},
	}
	for pose := range 3 {
		stageTestFace(t, cd, "Alice S", "alice@example.com", pose)
	}

	res, err := cd.finishRegistration(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("finishRegistration: %v", err)
	}
	if res["face_images"] != 3 {
		t.Fatalf("face_images = %v, want 3", res["face_images"])
	}
	if *recomputes != 1 {
		t.Fatalf("recompute_embeddings ran %d times, want 1", *recomputes)
	}
	pending, _ := cd.pendingDir("alice@example.com")
	if got := countFiles(t, pending); got != 0 {
		t.Fatalf("%d faces left staged after finish", got)
	}
	rec := cd.customers["alice@example.com"]
	if rec.Name != "Alice S" || len(rec.Orders) != 1 {
		t.Fatalf("record = %+v, want the new name and the kept order history", rec)
	}
	if _, err := os.Stat(cd.customersFilePath()); err != nil {
		t.Fatalf("customers.json not written: %v", err)
	}
}

func TestFinishRegistrationWithoutVisionLeavesFacesStaged(t *testing.T) {
	cd := newTestDetector(t)
	stageTestFace(t, cd, "Alice", "alice@example.com", 0)

	if _, err := cd.finishRegistration(context.Background(), "alice@example.com"); err == nil {
		t.Fatal("finishRegistration without a vision service succeeded, want an error")
	}
	pending, _ := cd.pendingDir("alice@example.com")
	if got := countFiles(t, pending); got != 1 {
		t.Fatalf("staged faces = %d, want the capture left staged", got)
	}
	if len(cd.customers) != 0 {
		t.Fatal("a failed finish saved a customer record")
	}
}
