// Package ops — MongoDB operational data access for the ops-api:
// fleet rollup from the device twin, command lifecycle tracking, and the
// unified events log. All reads go through aggregations so the dashboard
// stays a thin client (the "smart backend" story).
package ops

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Store owns the operational collections.
type Store struct {
	log       *slog.Logger
	db        *mongo.Database
	Devices   *mongo.Collection
	Twin      *mongo.Collection
	Commands  *mongo.Collection
	Events    *mongo.Collection
	Incidents *mongo.Collection
	Policy    *mongo.Collection
	Approvals *mongo.Collection

	firedWhile map[string]bool // alert-rule edge trigger: ruleId|deviceId -> currently holding
	rulesMu    sync.Mutex
}

// New wires a Store against the database.
func New(log *slog.Logger, db *mongo.Database) *Store {
	return &Store{
		log:        log,
		db:         db,
		Devices:    db.Collection("devices"),
		Twin:       db.Collection("device_state"),
		Commands:   db.Collection("commands"),
		Events:     db.Collection("events"),
		Incidents:  db.Collection("ai_incidents"),
		Policy:     db.Collection("ai_policy"),
		Approvals:  db.Collection("ai_approvals"),
		firedWhile: map[string]bool{},
	}
}

// DeviceRow is one row of the fleet grid (twin joined with the registry).
type DeviceRow struct {
	DeviceID    string     `bson:"deviceId"   json:"deviceId"`
	Site        string     `bson:"site"       json:"site"`
	Zone        string     `bson:"zone"       json:"zone"`
	Product     string     `bson:"product"    json:"product"`
	ValueAtRisk int        `bson:"valueAtRisk" json:"valueAtRisk"`
	Setpoint    float64    `bson:"setpoint"   json:"setpoint"`
	Reported    *bson.M    `bson:"reported"   json:"reported"`
	LastSeen    *time.Time `bson:"lastSeen"  json:"lastSeen"`
	Status      string     `bson:"status"     json:"status"`
}

// Status values (mirrors aggdb thresholds).
const (
	StatusOK     = "OK"
	StatusAtRisk = "AT_RISK"
	StatusAlarm  = "ALARM"
	StatusStale  = "STALE"
)

// atRiskBandC is the deadband applied above setpoint before a stopped
// compressor counts as "not cooling when it should be". A thermostat holds
// the cabinet within roughly ±0.2 °C, so a bare `tempC > setpoint` test is
// true for a healthy box resting at -17.9 against a -18 setpoint.
const atRiskBandC = 0.2

// atRiskWarmBandC is how far above setpoint a cabinet may drift before it is
// AT_RISK regardless of whether the compressor is running. A healthy
// thermostat never leaves ±0.08 °C; +2 °C means the box is losing the fight
// (failed compressor, wedged door, blocked condenser) even if the controller
// is still calling for cooling. Without this, a cabinet warming through
// -14.4 °C with the compressor running showed green until it crossed the
// fixed -14 °C line — the dashboard missed the exact failure the gateway's
// slope view was already flagging.
const atRiskWarmBandC = 2.0

// statusExpr is the shared OK/AT_RISK/ALARM/STALE switch expression (also
// reused by the series endpoint). Order matters: STALE first, then ALARM.
func statusExpr(now time.Time) bson.D {
	// {case: <pred>, then: <value>} helper keeps the nesting legible.
	branch := func(pred any, then string) bson.D {
		return bson.D{{Key: "case", Value: pred}, {Key: "then", Value: then}}
	}
	or := func(ops ...any) bson.D { return bson.D{{Key: "$or", Value: ops}} }
	and := func(ops ...any) bson.D { return bson.D{{Key: "$and", Value: ops}} }
	eq := func(a, b any) bson.D { return bson.D{{Key: "$eq", Value: bson.A{a, b}}} }
	gt := func(a, b any) bson.D { return bson.D{{Key: "$gt", Value: bson.A{a, b}}} }
	lt := func(a, b any) bson.D { return bson.D{{Key: "$lt", Value: bson.A{a, b}}} }
	ifNull := func(field, def any) bson.D {
		return bson.D{{Key: "$ifNull", Value: bson.A{field, def}}}
	}

	staleCase := branch(or(
		eq("$lastSeen", nil),
		lt("$lastSeen", now.Add(-30*time.Second)),
	), StatusStale)

	alarmCase := branch(gt(
		ifNull("$reported.tempC", -99), -10,
	), StatusAlarm)

	// AT_RISK = temp above the at-risk band, OR meaningfully above its
	// setpoint (whether or not the compressor is running), OR compressor off
	// while above the tight deadband.
	//
	// Three things matter here:
	//   - The setpoint lives at `desired.setpoint` on the twin, not at the top
	//     level. Comparing against a non-existent `$setpoint` silently fell
	//     back to a default, so every freezer resting at -17.9 counted as
	//     "above setpoint".
	//   - The comparisons need deadbands. A thermostat holds the cabinet
	//     within roughly ±0.2 °C, so `tempC > setpoint` is true for a
	//     perfectly healthy box sitting at -17.9 against a -18 setpoint.
	//   - A running compressor is not a clean bill of health: if the cabinet
	//     is more than +2 °C above setpoint it is losing ground anyway, and
	//     the fleet must show AT_RISK like the gateway health view does.
	//
	// The setpoint lives at `desired.setpoint` (operator override). The
	// registry's top-level `setpoint` is the configured default and must be
	// the fallback: ingest owns the twin and writes `reported` only, so a
	// device nobody has re-set has no `desired` subdocument at all. Without
	// this fallback the comparison ran against 99 and the branch was dead —
	// the fleet stayed green while a failed compressor warmed the cabinet
	// (only the gateway's slope view caught it).
	setpoint := bson.D{{Key: "$ifNull", Value: bson.A{"$desired.setpoint", "$setpoint", 99}}}
	warm := func(band float64) bson.D {
		return gt("$reported.tempC", bson.D{{Key: "$add", Value: bson.A{setpoint, band}}})
	}
	// Missing setpoint everywhere is treated as unknown, which keeps the box
	// OK — a reporting gap must not invent a fault.
	atRiskCase := branch(or(
		gt(ifNull("$reported.tempC", -99), -14),
		warm(atRiskWarmBandC),
		and(
			eq(ifNull("$reported.compressorRunning", true), false),
			warm(atRiskBandC),
		),
	), StatusAtRisk)

	return bson.D{{Key: "$switch", Value: bson.D{
		{Key: "branches", Value: bson.A{staleCase, alarmCase, atRiskCase}},
		{Key: "default", Value: StatusOK},
	}}}
}

// Fleet returns every device: registry metadata joined with the twin's
// reported state via $lookup. Status derives from the reported temperature.
func (s *Store) Fleet(ctx context.Context) ([]DeviceRow, error) {
	now := time.Now().UTC()
	cur, err := s.Devices.Aggregate(ctx, bson.A{
		bson.D{{Key: "$sort", Value: bson.D{{Key: "deviceId", Value: 1}}}},
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "device_state"},
			{Key: "localField", Value: "deviceId"},
			{Key: "foreignField", Value: "deviceId"},
			{Key: "as", Value: "state"},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "lastSeen", Value: bson.D{{Key: "$ifNull", Value: bson.A{
				bson.D{{Key: "$arrayElemAt", Value: bson.A{"$state.lastSeen", 0}}}, nil,
			}}}},
			{Key: "reported", Value: bson.D{{Key: "$ifNull", Value: bson.A{
				bson.D{{Key: "$arrayElemAt", Value: bson.A{"$state.reported", 0}}}, bson.M{},
			}}}},
			// Desired setpoint, needed by the AT_RISK rule: "compressor off
			// while the box is above setpoint" must compare against the real
			// commanded setpoint, not a hard-coded default. Fall back to the
			// registry setpoint so the twin view (and the API payload the
			// console renders) is complete even before ingest/initdb seeds
			// `desired` on a brand-new device.
			{Key: "desired", Value: bson.D{{Key: "$ifNull", Value: bson.A{
				bson.D{{Key: "$arrayElemAt", Value: bson.A{"$state.desired", 0}}},
				bson.D{{Key: "setpoint", Value: "$setpoint"}},
			}}}},
		}}},
		// status must be computed in its OWN stage: within a single $addFields
		// the switch would see lastSeen before it is computed (missing field
		// => {$eq:[missing,null]} is true => everything STALE).
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "status", Value: statusExpr(now)},
		}}},
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "state", Value: 0},
		}}},
	})
	if err != nil {
		return nil, err
	}
	var rows []DeviceRow
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Command is one entry of the command lifecycle collection.
type Command struct {
	ID        bson.ObjectID `bson:"_id,omitempty"  json:"id,omitempty"`
	CommandID string        `bson:"commandId"      json:"commandId"`
	DeviceID  string        `bson:"deviceId"       json:"deviceId"`
	Action    string        `bson:"action"         json:"action"`
	Args      bson.M        `bson:"args,omitempty" json:"args,omitempty"`
	Status    string        `bson:"status"         json:"status"` // QUEUED|SENT|ACKED|FAILED|TIMEOUT
	IssuedBy  string        `bson:"issuedBy"       json:"issuedBy"`
	LatencyMs *int64        `bson:"latencyMs,omitempty" json:"latencyMs,omitempty"`
	AckDetail string        `bson:"ackDetail,omitempty" json:"ackDetail,omitempty"`
	CreatedAt time.Time     `bson:"createdAt"      json:"createdAt"`
	SentAt    *time.Time    `bson:"sentAt,omitempty"    json:"sentAt,omitempty"`
	AckedAt   *time.Time    `bson:"ackedAt,omitempty"   json:"ackedAt,omitempty"`
	ExpiresAt time.Time     `bson:"expiresAt"      json:"expiresAt"`
}

// Command statuses.
const (
	CmdQueued  = "QUEUED"
	CmdSent    = "SENT"
	CmdAcked   = "ACKED"
	CmdFailed  = "FAILED"
	CmdTimeout = "TIMEOUT"
)

// CreateCommand records a QUEUED command (unique commandId).
func (s *Store) CreateCommand(ctx context.Context, cmd *Command) error {
	_, err := s.Commands.InsertOne(ctx, cmd)
	return err
}

// MarkSent flips a command to SENT (published to the broker). It only moves
// QUEUED commands: a fast device ack can land before this update runs, and
// rewriting ACKED back to SENT would leave the command stuck until the
// expiry sweeper (and lose the measured ack latency).
func (s *Store) MarkSent(ctx context.Context, commandID string) {
	now := time.Now().UTC()
	_, _ = s.Commands.UpdateOne(ctx,
		bson.M{"commandId": commandID, "status": CmdQueued},
		bson.M{"$set": bson.M{"status": CmdSent, "sentAt": now}})
}

// ApplyAck applies a device ack; latency is measured from sentAt, falling
// back to createdAt when the ack beats the SENT write (sub-millisecond acks
// from an in-process simulator do exactly that).
func (s *Store) ApplyAck(ctx context.Context, commandID string, ok bool, detail string, ackAt time.Time) bool {
	var cmd Command
	if err := s.Commands.FindOne(ctx, bson.M{"commandId": commandID}).Decode(&cmd); err != nil {
		return false
	}
	set := bson.M{
		"status":    CmdAcked,
		"ackedAt":   ackAt,
		"ackDetail": detail,
	}
	if !ok {
		set["status"] = CmdFailed
	}
	base := cmd.CreatedAt
	if cmd.SentAt != nil {
		base = *cmd.SentAt
	} else {
		set["sentAt"] = ackAt
	}
	set["latencyMs"] = ackAt.Sub(base).Milliseconds()
	_, _ = s.Commands.UpdateOne(ctx, bson.M{"commandId": commandID}, bson.M{"$set": set})
	s.AddEvent(ctx, Event{
		Type: "command_" + set["status"].(string), DeviceID: cmd.DeviceID,
		Message: cmd.Action + ": " + detail, Actor: "device",
		CommandID: commandID, At: ackAt,
	})
	return true
}

// TimeoutStale marks SENT commands older than their expiry as TIMEOUT.
func (s *Store) TimeoutStale(ctx context.Context) {
	cur, err := s.Commands.Find(ctx, bson.M{
		"status":    bson.M{"$in": bson.A{CmdQueued, CmdSent}},
		"expiresAt": bson.M{"$lt": time.Now().UTC()},
	})
	if err != nil {
		return
	}
	var stale []Command
	_ = cur.All(ctx, &stale)
	for _, c := range stale {
		_, _ = s.Commands.UpdateOne(ctx, bson.M{"_id": c.ID},
			bson.M{"$set": bson.M{"status": CmdTimeout}})
		s.AddEvent(ctx, Event{
			Type: "command_TIMEOUT", DeviceID: c.DeviceID,
			Message: c.Action + " timed out", Actor: "system", CommandID: c.CommandID,
			At: time.Now().UTC(),
		})
	}
}

// Event is one entry of the unified audit log.
type Event struct {
	ID         bson.ObjectID `bson:"_id,omitempty"      json:"id,omitempty"`
	Type       string        `bson:"type"               json:"type"` // device_fault|command_*|ai_decision|approval|operator|recovery
	DeviceID   string        `bson:"deviceId,omitempty" json:"deviceId,omitempty"`
	Message    string        `bson:"message"            json:"message"`
	Actor      string        `bson:"actor"              json:"actor"` // ai-agent|operator:<name>|system|device
	CommandID  string        `bson:"commandId,omitempty" json:"commandId,omitempty"`
	IncidentID string        `bson:"incidentId,omitempty" json:"incidentId,omitempty"`
	Severity   string        `bson:"severity,omitempty" json:"severity,omitempty"` // info|warning|critical
	Data       bson.M        `bson:"data,omitempty"     json:"data,omitempty"`
	At         time.Time     `bson:"at"                 json:"at"`
	ExpiresAt  time.Time     `bson:"expiresAt"          json:"expiresAt"`
}

// AddEvent writes one audit event (expires in 7 days).
func (s *Store) AddEvent(ctx context.Context, e Event) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.ExpiresAt = e.At.Add(7 * 24 * time.Hour)
	if e.Severity == "" {
		e.Severity = "info"
	}
	if _, err := s.Events.InsertOne(ctx, e); err != nil {
		s.log.Warn("event insert failed", "type", e.Type, "err", err)
	}
}

// Events lists the latest audit events, optionally filtered by device/type.
func (s *Store) ListEvents(ctx context.Context, deviceID, typ string, limit int64) ([]Event, error) {
	q := bson.M{}
	if deviceID != "" {
		q["deviceId"] = deviceID
	}
	if typ != "" {
		q["type"] = typ
	}
	cur, err := s.Events.Find(ctx, q,
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	var out []Event
	return out, cur.All(ctx, &out)
}
