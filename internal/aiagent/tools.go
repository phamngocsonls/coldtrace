// Package aiagent — read-only tools the chat model may call to ground its
// answers in live MongoDB facts. The tool surface mirrors the ops-api REST
// contract but NEVER mutates: the agent explains and proposes, humans
// execute (human-in-the-loop by design).
package aiagent

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/onemount/coldtrace/internal/aggdb"
)

// toolDef is the OpenAI function-calling schema for one tool.
type toolDef struct {
	Type     string         `json:"type"`
	Function toolDefWrapper `json:"function"`
}

type toolDefWrapper struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// agentTools is the catalog handed to the model on every grounded turn.
var agentTools = []toolDef{
	{
		Type: "function",
		Function: toolDefWrapper{
			Name:        "get_fleet",
			Description: "List every freezer device with live status (OK/AT_RISK/ALARM/STALE), temperature, slope and value at risk.",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	},
	{
		Type: "function",
		Function: toolDefWrapper{
			Name:        "get_device",
			Description: "One device's registry entry, device twin (reported + desired) and recent commands.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"deviceId": map[string]any{"type": "string", "description": "e.g. FZ-003"},
				},
				"required": []string{"deviceId"},
			},
		},
	},
	{
		Type: "function",
		Function: toolDefWrapper{
			Name:        "list_incidents",
			Description: "Recent AI incidents (fault class, confidence, ETA to alarm, decision, status).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "string", "enum": []string{"OPEN", "RESOLVED"}},
				},
			},
		},
	},
	{
		Type: "function",
		Function: toolDefWrapper{
			Name:        "telemetry_stats",
			Description: "Per-device telemetry stats for the last N minutes: min/max/avg temp, excursion minutes above -14C.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"deviceId":  map[string]any{"type": "string"},
					"windowMin": map[string]any{"type": "integer", "default": 15, "maximum": 1440},
				},
				"required": []string{"deviceId"},
			},
		},
	},
}

// runTool executes one tool call against MongoDB (read-only, 5s budget).
func runTool(ctx context.Context, db *mongo.Database, name, argsJSON string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var args struct {
		DeviceID  string `json:"deviceId"`
		Status    string `json:"status"`
		WindowMin int    `json:"windowMin"`
	}
	if argsJSON != "" {
		if err := bson.UnmarshalExtJSON([]byte(argsJSON), false, &args); err != nil {
			return "", fmt.Errorf("bad tool args: %w", err)
		}
	}
	switch name {
	case "get_fleet":
		return toolFleet(ctx, db)
	case "get_device":
		return toolDevice(ctx, db, args.DeviceID)
	case "list_incidents":
		return toolIncidents(ctx, db, args.Status)
	case "telemetry_stats":
		if args.DeviceID == "" {
			return "", fmt.Errorf("deviceId required")
		}
		if args.WindowMin <= 0 {
			args.WindowMin = 15
		}
		return toolTelemetry(ctx, db, args.DeviceID, time.Duration(args.WindowMin)*time.Minute)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func toolFleet(ctx context.Context, db *mongo.Database) (string, error) {
	// same shape as GET /api/fleet: registry joined with the device_state
	// twin (reported/lastSeen live in device_state, NOT in devices) —
	// $lookup then derive status from the twin, mirroring ops.Store.Fleet.
	cur, err := db.Collection("devices").Aggregate(ctx, bson.A{
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "device_state"},
			{Key: "localField", Value: "deviceId"},
			{Key: "foreignField", Value: "deviceId"},
			{Key: "as", Value: "state"},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "reported", Value: bson.D{{Key: "$ifNull", Value: bson.A{
				bson.D{{Key: "$arrayElemAt", Value: bson.A{"$state.reported", 0}}}, bson.M{},
			}}}},
			{Key: "lastSeen", Value: bson.D{{Key: "$ifNull", Value: bson.A{
				bson.D{{Key: "$arrayElemAt", Value: bson.A{"$state.lastSeen", 0}}}, nil,
			}}}},
		}}},
		bson.D{{Key: "$limit", Value: 50}},
	})
	if err != nil {
		return "", err
	}
	type row struct {
		DeviceID string     `bson:"deviceId" json:"deviceId"`
		Site     string     `bson:"site"     json:"site"`
		Value    int64      `bson:"valueAtRisk" json:"valueAtRisk"`
		Reported bson.M     `bson:"reported" json:"reported"`
		LastSeen *time.Time `bson:"lastSeen" json:"lastSeen"`
		Status   string     `json:"status"`
		Temp     *float64   `json:"tempC"`
	}
	var out []row
	for cur.Next(ctx) {
		var r row
		if err := cur.Decode(&r); err != nil {
			continue
		}
		r.Status, r.Temp = deriveStatus(r.Reported, r.LastSeen)
		out = append(out, r)
	}
	cur.Close(ctx)
	return mustJSON(out), nil
}

func toolDevice(ctx context.Context, db *mongo.Database, deviceID string) (string, error) {
	var dev, twin bson.M
	if err := db.Collection("devices").FindOne(ctx, bson.M{"deviceId": deviceID}).Decode(&dev); err != nil {
		return "", fmt.Errorf("device %s not found", deviceID)
	}
	_ = db.Collection("device_state").FindOne(ctx, bson.M{"deviceId": deviceID}).Decode(&twin)
	cur, err := db.Collection("commands").Find(ctx, bson.M{"deviceId": deviceID},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(5))
	var cmds []bson.M
	if err == nil && cur != nil {
		_ = cur.All(ctx, &cmds)
		cur.Close(ctx)
	}
	return mustJSON(bson.M{"device": dev, "twin": twin, "recentCommands": cmds}), nil
}

func toolIncidents(ctx context.Context, db *mongo.Database, status string) (string, error) {
	q := bson.M{}
	if status != "" {
		q["status"] = status
	}
	cur, err := db.Collection("ai_incidents").Find(ctx, q,
		options.Find().SetSort(bson.D{{Key: "openedAt", Value: -1}}).SetLimit(20))
	if err != nil {
		return "", err
	}
	var out []bson.M
	if err := cur.All(ctx, &out); err != nil {
		return "", err
	}
	return mustJSON(out), nil
}

func toolTelemetry(ctx context.Context, db *mongo.Database, deviceID string, window time.Duration) (string, error) {
	if window < time.Minute {
		window = time.Minute
	}
	if window > 24*time.Hour {
		window = 24 * time.Hour
	}
	since := time.Now().UTC().Add(-window)
	// DedupByTS first: MQTT QoS 1 redelivery writes the same (deviceId, ts)
	// twice and $setWindowFields hard-errors on repeated sortBy values per
	// partition — the RCA tool would 500 on a broker blip. The stage
	// restores ts ordering itself.
	pipeline := append(bson.A{
		bson.D{{Key: "$match", Value: bson.M{"meta.deviceId": deviceID, "ts": bson.M{"$gte": since}}}},
	}, aggdb.DedupByTS()...)
	pipeline = append(pipeline,
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "partitionBy", Value: "$meta.deviceId"},
			{Key: "sortBy", Value: bson.D{{Key: "ts", Value: 1}}},
			{Key: "output", Value: bson.D{
				{Key: "prevTs", Value: bson.D{{Key: "$shift", Value: bson.D{
					{Key: "output", Value: "$ts"},
					{Key: "by", Value: -1},
				}}}},
			}},
		}}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "exc", Value: bson.D{{Key: "$cond", Value: bson.A{
				bson.D{{Key: "$gt", Value: bson.A{"$tempC", -14}}},
				// span covered by this excursion sample, capped at 10s so a
				// device gap stops the clock instead of inflating minutes
				bson.D{{Key: "$cond", Value: bson.A{
					bson.D{{Key: "$and", Value: bson.A{
						bson.D{{Key: "$ne", Value: bson.A{"$prevTs", nil}}},
						bson.D{{Key: "$lte", Value: bson.A{
							bson.D{{Key: "$subtract", Value: bson.A{"$ts", "$prevTs"}}},
							10000,
						}}},
					}}},
					bson.D{{Key: "$subtract", Value: bson.A{"$ts", "$prevTs"}}},
					0,
				}}},
				0,
			}}}},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$meta.deviceId"},
			{Key: "minTemp", Value: bson.D{{Key: "$min", Value: "$tempC"}}},
			{Key: "maxTemp", Value: bson.D{{Key: "$max", Value: "$tempC"}}},
			{Key: "avgTemp", Value: bson.D{{Key: "$avg", Value: "$tempC"}}},
			{Key: "samples", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "excursionMs", Value: bson.D{{Key: "$sum", Value: "$exc"}}},
			{Key: "lastTs", Value: bson.D{{Key: "$max", Value: "$ts"}}},
		}}},
	)
	cur, err := db.Collection("telemetry").Aggregate(ctx, pipeline)
	if err != nil {
		return "", err
	}
	var out []bson.M
	if err := cur.All(ctx, &out); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return mustJSON(bson.M{"deviceId": deviceID, "note": "no telemetry in window"}), nil
	}
	doc := out[0]
	// excursionMs decodes as int32/int64/float64 depending on driver
	// coercion — normalize before converting (never panic on types).
	var ms float64
	if raw, ok := doc["excursionMs"]; ok {
		switch v := raw.(type) {
		case int32:
			ms = float64(v)
		case int64:
			ms = float64(v)
		case float64:
			ms = v
		}
	}
	doc["excursionMin"] = ms / 60000
	delete(doc, "excursionMs")
	doc["windowMinutes"] = int(window.Minutes())
	return mustJSON(doc), nil
}

// deriveStatus mirrors the ops-api status rule (statusExpr in
// internal/ops/store.go): lastSeen missing or older than 30s -> STALE,
// ALARM > -10C, AT_RISK > -14C, else OK. A missing tempC reads as STALE
// (0°C would be ALARM — a reading of zero is never real for a freezer).
// `reported` is ALREADY the twin's reported subdoc (the $lookup in
// toolFleet unwraps it) — unwrap once.
func deriveStatus(reported bson.M, lastSeen *time.Time) (string, *float64) {
	now := time.Now().UTC()
	if lastSeen == nil || now.Sub(*lastSeen) > 30*time.Second {
		return "STALE", nil
	}
	t, ok := reported["tempC"].(float64)
	if !ok {
		return "STALE", nil // no reading: unknown, not a fake 0°C
	}
	switch {
	case t > -10:
		return "ALARM", &t
	case t > -14:
		return "AT_RISK", &t
	default:
		return "OK", &t
	}
}
