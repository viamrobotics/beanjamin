// Where the fleet's tabular data lives. Kept apart from home/data.ts, which
// loads the Viam SDK eagerly, so the kiosk can query without pulling it in.

export const ORG_ID = "e76d1b3b-0468-4efd-bb7f-fb1d2b352fcb";
export const LOCATION_ID = "oeq47g5p1m";
/** The coffee service's order_sensor_name component: one reading per order. */
export const RESOURCE_NAME = "order-events";
/** The coffee service's issue_sensor_name component: one reading per issue report. */
export const ISSUES_RESOURCE_NAME = "order-issues";
