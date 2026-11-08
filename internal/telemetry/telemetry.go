package telemetry

import "time"

// Meta is the time series metaField: the labels that identify WHICH sensor
// produced a reading. Per the MongoDB time series brief (slide 11) the
// metaField should hold the source id plus fixed labels you filter on and
// that rarely change — never a measured value or a per-reading id.
//
// This is the one field MongoDB cannot change after collection creation, so
// the shape is load-bearing: all readings with an equal Meta form one series
// and share buckets, which is what lets `{"meta.site": "OM-DNG-01"}` skip
// whole buckets instead of scanning.
type Meta struct {
	DeviceID string `bson:"deviceId" json:"deviceId"`
	Site     string `bson:"site,omitempty"     json:"site,omitempty"`
	Zone     string `bson:"zone,omitempty"     json:"zone,omitempty"`
}

// Telemetry is one sensor reading published by a freezer device.
// Stored in the MongoDB time series collection `telemetry`
// (timeField=ts, metaField=meta, expireAfterSeconds=86400).
type Telemetry struct {
	TS                time.Time `bson:"ts"   json:"ts"`
	Meta              Meta      `bson:"meta" json:"meta"`
	TempC             float64   `bson:"tempC"             json:"tempC"`
	PowerDraw         float64   `bson:"powerDraw"         json:"powerDraw"`
	CompressorRunning bool      `bson:"compressorRunning" json:"compressorRunning"`
	DoorOpen          bool      `bson:"doorOpen"          json:"doorOpen"`
	Seq               int64     `bson:"seq"               json:"seq"`
}
