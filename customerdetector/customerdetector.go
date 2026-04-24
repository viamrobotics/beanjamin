// Package customerdetector registers a viam:beanjamin:customer-detector model
// that implements the rdk:service:generic API. It orchestrates the
// viam:vision:face-identification service to register and identify return
// customers by associating captured face embeddings with email addresses.
package customerdetector

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/vision"
)

// Model is the full model triplet for this service.
var Model = resource.NewModel("viam", "beanjamin", "customer-detector")

// knownFacesDir is the subdirectory under DataDir where face images are stored.
const knownFacesDir = "known_faces"

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newCustomerDetector,
		},
	)
}

// Config describes the required and optional attributes for the customer-detector.
type Config struct {
	CameraName          string  `json:"camera_name"`
	VisionServiceName   string  `json:"vision_service_name"`
	DataDir             string  `json:"data_dir"`
	ConfidenceThreshold float64 `json:"confidence_threshold,omitempty"`
	MinFaceAreaFraction float64 `json:"min_face_area_fraction,omitempty"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.CameraName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "camera_name")
	}
	if cfg.VisionServiceName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "vision_service_name")
	}
	if cfg.DataDir == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "data_dir")
	}
	return []string{
			camera.Named(cfg.CameraName).String(),
		}, []string{
			vision.Named(cfg.VisionServiceName).String(),
		}, nil
}

// customerRecord stores metadata about a registered customer.
type customerRecord struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	ImageDir string `json:"image_dir"`
}

type customerDetector struct {
	resource.AlwaysRebuild

	name                resource.Name
	logger              logging.Logger
	camera              camera.Camera
	dataDir             string
	threshold           float64
	minFaceAreaFraction float64
	visionName          string

	mu        sync.RWMutex
	customers map[string]*customerRecord // keyed by email
	vision    vision.Service             // lazily resolved; may be nil at startup
}

func newCustomerDetector(
	ctx context.Context,
	deps resource.Dependencies,
	rawConf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	cam, err := camera.FromProvider(deps, conf.CameraName)
	if err != nil {
		return nil, fmt.Errorf("camera %q not found in dependencies: %w", conf.CameraName, err)
	}

	// Ensure data directories exist before the face-identification module
	// starts — it crashes if picture_directory is missing.
	facesDir := filepath.Join(conf.DataDir, knownFacesDir)
	if err := os.MkdirAll(facesDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create known_faces directory: %w", err)
	}

	threshold := conf.ConfidenceThreshold
	if threshold == 0 {
		threshold = 0.5
	}

	minFaceAreaFraction := conf.MinFaceAreaFraction
	if minFaceAreaFraction == 0 {
		minFaceAreaFraction = defaultMinFaceAreaFraction
	}

	// Vision service is an optional dependency — it may not be ready yet
	// (e.g. face-identification needed this directory to exist first).
	vis, _ := vision.FromProvider(deps, conf.VisionServiceName)

	cd := &customerDetector{
		name:                rawConf.ResourceName(),
		logger:              logger,
		camera:              cam,
		vision:              vis,
		visionName:          conf.VisionServiceName,
		dataDir:             conf.DataDir,
		threshold:           threshold,
		minFaceAreaFraction: minFaceAreaFraction,
		customers:           make(map[string]*customerRecord),
	}

	if err := cd.loadCustomers(); err != nil {
		logger.Warnf("failed to load existing customers: %v", err)
	}

	return cd, nil
}

func (cd *customerDetector) Name() resource.Name {
	return cd.name
}

func (cd *customerDetector) getVision() (vision.Service, error) {
	cd.mu.RLock()
	vis := cd.vision
	cd.mu.RUnlock()
	if vis != nil {
		return vis, nil
	}
	return nil, fmt.Errorf("vision service %q is not available yet", cd.visionName)
}

func (cd *customerDetector) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if reg, ok := cmd["register_customer"].(map[string]interface{}); ok {
		email, _ := reg["email"].(string)
		name, _ := reg["name"].(string)
		return cd.registerCustomer(ctx, name, email)
	}
	if email, ok := cmd["finish_registration"].(string); ok {
		return cd.finishRegistration(ctx, email)
	}
	if _, ok := cmd["identify_customer"]; ok {
		return cd.identifyCustomer(ctx)
	}
	if _, ok := cmd["list_customers"]; ok {
		return cd.listCustomers()
	}
	if email, ok := cmd["remove_customer"].(string); ok {
		return cd.removeCustomer(ctx, email)
	}
	if _, ok := cmd["get_info"]; ok {
		return map[string]interface{}{
			"camera_name": cd.camera.Name().ShortName(),
		}, nil
	}
	return nil, fmt.Errorf("unknown command, supported: register_customer, finish_registration, identify_customer, list_customers, remove_customer, get_info")
}

// registerCustomer captures an image from the camera and stores it as a known
// face for the given email address. It then tells the vision service to
// recompute its embeddings so the new face is immediately recognisable.
func (cd *customerDetector) registerCustomer(ctx context.Context, name, email string) (map[string]interface{}, error) {
	if email == "" {
		return nil, fmt.Errorf("email must not be empty")
	}
	if name == "" {
		return nil, fmt.Errorf("name must not be empty")
	}

	// Capture an image from the camera.
	img, err := camera.DecodeImageFromCamera(ctx, cd.camera, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to capture image: %w", err)
	}
	img = centerCrop(img)

	// Save the image into the known_faces directory under the customer's email.
	customerDir := filepath.Join(cd.dataDir, knownFacesDir, email)
	if err := os.MkdirAll(customerDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create customer directory: %w", err)
	}

	// Count existing images to generate a unique filename.
	entries, _ := os.ReadDir(customerDir)
	filename := fmt.Sprintf("face_%d.jpeg", len(entries)+1)
	imgPath := filepath.Join(customerDir, filename)

	f, err := os.Create(imgPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create face image file: %w", err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to encode face image: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("failed to write face image: %w", err)
	}

	cd.logger.Infof("saved face image for %q at %s", email, imgPath)

	// Persist customer record.
	cd.mu.Lock()
	cd.customers[email] = &customerRecord{
		Name:     name,
		Email:    email,
		ImageDir: customerDir,
	}
	cd.mu.Unlock()

	if err := cd.saveCustomers(); err != nil {
		return nil, fmt.Errorf("failed to persist customer records: %w", err)
	}

	return map[string]interface{}{
		"registered": email,
		"name":       name,
		"image_path": imgPath,
	}, nil
}

// finishRegistration signals that the app is done capturing face images for
// a customer. It triggers the vision service to recompute its embeddings so
// all the newly captured faces become recognisable.
func (cd *customerDetector) finishRegistration(ctx context.Context, email string) (map[string]interface{}, error) {
	if email == "" {
		return nil, fmt.Errorf("email must not be empty")
	}

	cd.mu.RLock()
	rec, exists := cd.customers[email]
	cd.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("customer %q not found — call register_customer first", email)
	}

	entries, _ := os.ReadDir(rec.ImageDir)

	vis, err := cd.getVision()
	if err != nil {
		return nil, err
	}

	if _, err := vis.DoCommand(ctx, map[string]interface{}{
		"command": "recompute_embeddings",
	}); err != nil {
		return nil, fmt.Errorf("failed to recompute embeddings: %w", err)
	}

	cd.logger.Infof("finished registration for %q with %d face images", email, len(entries))

	return map[string]interface{}{
		"email":       email,
		"name":        rec.Name,
		"face_images": len(entries),
	}, nil
}

// defaultMinFaceAreaFraction is the minimum fraction of the (center-cropped)
// image area a face bounding box must cover to be considered a valid identify
// candidate. 0.08 corresponds to a face spanning ~28% of the frame linearly.
const defaultMinFaceAreaFraction = 0.08

// identifyCustomer captures an image and runs face detection against known
// customers, returning the best match above the confidence threshold whose
// bounding box covers at least min_face_area_fraction of the image. When
// multiple detections qualify, the largest (by bounding-box area) wins.
func (cd *customerDetector) identifyCustomer(ctx context.Context) (map[string]interface{}, error) {
	vis, err := cd.getVision()
	if err != nil {
		return nil, err
	}

	img, err := camera.DecodeImageFromCamera(ctx, cd.camera, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to capture image: %w", err)
	}

	img = centerCrop(img)

	bounds := img.Bounds()
	imageArea := bounds.Dx() * bounds.Dy()
	if imageArea == 0 {
		return nil, fmt.Errorf("captured image has zero area")
	}

	detections, err := vis.Detections(ctx, img, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get detections: %w", err)
	}

	var bestLabel string
	var bestConf float64
	var bestArea int
	for _, d := range detections {
		if d.Score() < cd.threshold {
			continue
		}
		bb := d.BoundingBox()
		if bb == nil {
			continue
		}
		area := bb.Dx() * bb.Dy()
		if float64(area)/float64(imageArea) < cd.minFaceAreaFraction {
			continue
		}
		if area > bestArea {
			bestArea = area
			bestConf = d.Score()
			bestLabel = d.Label()
		}
	}

	if bestLabel == "" {
		return map[string]interface{}{
			"identified":     false,
			"message":        "no known customer detected",
			"num_detections": len(detections),
		}, nil
	}

	cd.mu.RLock()
	rec, isRegistered := cd.customers[bestLabel]
	cd.mu.RUnlock()

	result := map[string]interface{}{
		"identified":    true,
		"email":         bestLabel,
		"confidence":    bestConf,
		"is_registered": isRegistered,
	}
	if isRegistered {
		result["name"] = rec.Name
	}
	return result, nil
}

func (cd *customerDetector) listCustomers() (map[string]interface{}, error) {
	cd.mu.RLock()
	defer cd.mu.RUnlock()

	customers := make([]map[string]interface{}, 0, len(cd.customers))
	for _, rec := range cd.customers {
		customers = append(customers, map[string]interface{}{
			"name":  rec.Name,
			"email": rec.Email,
		})
	}

	return map[string]interface{}{
		"customers": customers,
		"count":     len(customers),
	}, nil
}

func (cd *customerDetector) removeCustomer(ctx context.Context, email string) (map[string]interface{}, error) {
	cd.mu.Lock()
	rec, exists := cd.customers[email]
	if !exists {
		cd.mu.Unlock()
		return nil, fmt.Errorf("customer %q not found", email)
	}
	delete(cd.customers, email)
	cd.mu.Unlock()

	// Remove the face images directory.
	if err := os.RemoveAll(rec.ImageDir); err != nil {
		cd.logger.Warnf("failed to remove images for %q: %v", email, err)
	}

	if err := cd.saveCustomers(); err != nil {
		cd.logger.Warnf("failed to persist customer records: %v", err)
	}

	// Recompute embeddings so the vision service forgets this face.
	if vis, err := cd.getVision(); err == nil {
		if _, err := vis.DoCommand(ctx, map[string]interface{}{
			"command": "recompute_embeddings",
		}); err != nil {
			cd.logger.Warnf("failed to recompute embeddings after removing %q: %v", email, err)
		}
	}

	return map[string]interface{}{
		"removed": email,
	}, nil
}

// customersFilePath returns the path to the JSON file that persists customer records.
func (cd *customerDetector) customersFilePath() string {
	return filepath.Join(cd.dataDir, "customers.json")
}

func (cd *customerDetector) loadCustomers() error {
	data, err := os.ReadFile(cd.customersFilePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	cd.mu.Lock()
	defer cd.mu.Unlock()
	return json.Unmarshal(data, &cd.customers)
}

func (cd *customerDetector) saveCustomers() error {
	cd.mu.RLock()
	data, err := json.MarshalIndent(cd.customers, "", "  ")
	cd.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(cd.customersFilePath(), data, 0o644)
}

func (cd *customerDetector) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (cd *customerDetector) Close(context.Context) error {
	return nil
}

func centerCrop(img image.Image) image.Image {
	b := img.Bounds()
	size := b.Dx()
	if b.Dy() < size {
		size = b.Dy()
	}
	x0 := b.Min.X + (b.Dx()-size)/2
	y0 := b.Min.Y + (b.Dy()-size)/2
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(dst, dst.Bounds(), img, image.Pt(x0, y0), draw.Src)
	return dst
}
