package report

import "fmt"

// MachineLogsURL constructs an app.viam.com deep-link to this machine's
// logs from the VIAM_MACHINE_ID and VIAM_PRIMARY_ORG_ID env vars Viam injects
// into cloud-connected modules. Returns "" when either is unset (e.g. a local
// or test machine not connected to the cloud), so callers can omit the link.
func MachineLogsURL(machineID, orgID string) string {
	if machineID == "" || orgID == "" {
		return ""
	}
	return fmt.Sprintf("https://app.viam.com/machine/%s/logs?org=%s", machineID, orgID)
}

// ClipDataURL constructs an app.viam.com data-page deep-link filtered to
// the order's video clip. The clip is tagged with the order ID (a UUID, so the
// tag filter alone uniquely identifies it); locationID — from VIAM_LOCATION_ID
// — scopes the view and orgID — from VIAM_PRIMARY_ORG_ID — scopes the org.
// robotName is intentionally omitted: there is no robot-name env var, and the
// UUID tag makes it redundant. Returns "" when locationID is empty (e.g. a
// local/test machine), so callers can omit the link. Note: the clip is uploaded
// asynchronously after the notification is sent, so the link may show no
// results for the first ~15-60s.
func ClipDataURL(locationID, orgID, orderID string) string {
	if locationID == "" {
		return ""
	}
	return fmt.Sprintf("https://app.viam.com/data/all?locationId=%s&tags=%s&view=media%s",
		locationID, orderID, orgQueryParam(orgID))
}

// PlanRequestDataURL constructs an app.viam.com data-page deep-link to the
// order's plan-request files, filtered by the order-ID tag (view=files keeps it
// distinct from the order's video clip; the reader can narrow further by the
// planning_failure or step tags). Returns "" when locationID is empty (e.g. a
// local/test machine). Like the clip link, the files sync asynchronously, so the
// page may be empty for the first sync interval.
func PlanRequestDataURL(locationID, orgID, orderID string) string {
	if locationID == "" {
		return ""
	}
	return fmt.Sprintf("https://app.viam.com/data/all?locationId=%s&tags=%s&view=files%s",
		locationID, orderID, orgQueryParam(orgID))
}

// orgQueryParam renders the "&org=..." suffix that pins an app.viam.com link to
// the owning org, so a reader who belongs to several orgs lands in the right one
// instead of whichever org their session last used. Returns "" when the org is
// unknown, leaving a link that still resolves in the reader's current org.
func orgQueryParam(orgID string) string {
	if orgID == "" {
		return ""
	}
	return "&org=" + orgID
}
