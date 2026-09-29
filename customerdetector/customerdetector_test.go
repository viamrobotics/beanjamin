package customerdetector

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/rdk/logging"
)

func newTestDetector(t *testing.T) *customerDetector {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, knownFacesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return &customerDetector{
		logger:    logging.NewTestLogger(t),
		dataDir:   dataDir,
		customers: make(map[string]*customerRecord),
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
		if _, err := cd.registerCustomer(context.Background(), "Mallory", email); err == nil {
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
