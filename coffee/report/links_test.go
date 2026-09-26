package report

import "testing"

func TestBuildPlanRequestDataURL(t *testing.T) {
	if got := PlanRequestDataURL("", "org1", "oid"); got != "" {
		t.Errorf("empty locationID: got %q, want \"\"", got)
	}
	want := "https://app.viam.com/data/all?locationId=loc1&tags=oid-42&view=files&org=org1"
	if got := PlanRequestDataURL("loc1", "org1", "oid-42"); got != want {
		t.Errorf("PlanRequestDataURL = %q, want %q", got, want)
	}
	// An unknown org still yields a usable link, just unscoped.
	want = "https://app.viam.com/data/all?locationId=loc1&tags=oid-42&view=files"
	if got := PlanRequestDataURL("loc1", "", "oid-42"); got != want {
		t.Errorf("PlanRequestDataURL without org = %q, want %q", got, want)
	}
}

func TestBuildClipDataURL(t *testing.T) {
	if got := ClipDataURL("", "org1", "oid"); got != "" {
		t.Errorf("empty locationID: got %q, want \"\"", got)
	}
	want := "https://app.viam.com/data/all?locationId=loc1&tags=oid-42&view=media&org=org1"
	if got := ClipDataURL("loc1", "org1", "oid-42"); got != want {
		t.Errorf("ClipDataURL = %q, want %q", got, want)
	}
	want = "https://app.viam.com/data/all?locationId=loc1&tags=oid-42&view=media"
	if got := ClipDataURL("loc1", "", "oid-42"); got != want {
		t.Errorf("ClipDataURL without org = %q, want %q", got, want)
	}
}
