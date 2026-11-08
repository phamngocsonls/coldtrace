// Package ops — insights.go: per-site rollup demonstrating MongoDB's
// aggregation toolkit: $facet (multi-branch), $lookup (registry join),
// $setWindowFields/$derivative (trend), $group (rollup).
package ops

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// SiteInsight is one site's rollup for the console "Sites" strip.
type SiteInsight struct {
	Site            string  `json:"site"`
	Devices         int     `json:"devices"`
	OK              int     `json:"ok"`
	AtRisk          int     `json:"atRisk"`
	Alarm           int     `json:"alarm"`
	OpenIncidents   int     `json:"openIncidents"`
	ValueAtRisk     int64   `json:"valueAtRisk"`
	MaxSlopeCPM     float64 `json:"maxSlopeCpm"`
	AvgExcursionMin float64 `json:"avgExcursionMin"`
	MTTRMin         float64 `json:"mttrMin"`
}

// SiteInsights returns a per-site rollup assembled from four aggregation
// branches over one $facet:
//
//	statusRows    — devices + twin status (statusExpr switch) + $group by site
//	trendRows     — $setWindowFields + $derivative worst 2-min temp trend
//	incidentRows  — open count + MTTR ($avg of resolvedAt-openedAt)
//	excursionRows — minutes above -14°C last 15 min
//
// Cross-collection branches can't share one $facet (facet stages run against
// the input collection only — trend/excursion rows would be empty because
// devices has no `ts`/`tempC`). Each branch therefore aggregates its own
// collection (telemetry for trend+excursions, ai_incidents for incidents)
// and the results are joined in Go — same one-round-trip shape, correct data.
func (s *Store) SiteInsights(ctx context.Context) ([]SiteInsight, error) {
	now := time.Now().UTC()
	windowStart := now.Add(-15 * time.Minute)

	statusRows := bson.A{
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
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "status", Value: statusExpr(now)},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$site"},
			{Key: "devices", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "ok", Value: sumIf("status", StatusOK)},
			{Key: "atRisk", Value: sumIf("status", StatusAtRisk)},
			{Key: "alarm", Value: sumIf("status", StatusAlarm)},
			{Key: "valueAtRisk", Value: bson.D{{Key: "$sum", Value: "$valueAtRisk"}}},
		}}},
	}

	trendRows := bson.A{
		bson.D{{Key: "$match", Value: bson.M{"ts": bson.M{"$gte": windowStart}}}},
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "partitionBy", Value: "$meta.deviceId"},
			{Key: "sortBy", Value: bson.D{{Key: "ts", Value: 1}}},
			{Key: "output", Value: bson.D{
				{Key: "slope", Value: bson.D{
					{Key: "$derivative", Value: bson.D{{Key: "input", Value: "$tempC"}, {Key: "unit", Value: "minute"}}},
					{Key: "window", Value: bson.D{{Key: "range", Value: bson.A{-2, 0}}, {Key: "unit", Value: "minute"}}},
				}},
			}},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$meta.deviceId"},
			{Key: "maxSlope", Value: bson.D{{Key: "$max", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$slope", 0}}}}}},
		}}},
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "devices"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "deviceId"},
			{Key: "as", Value: "dev"},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "site", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$dev.site", 0}}}},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$site"},
			{Key: "maxSlopeCpm", Value: bson.D{{Key: "$max", Value: "$maxSlope"}}},
		}}},
	}

	incidentRows := bson.A{
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "devices"},
			{Key: "localField", Value: "deviceId"},
			{Key: "foreignField", Value: "deviceId"},
			{Key: "as", Value: "dev"},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "site", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$dev.site", 0}}}},
			{Key: "mttrMin", Value: bson.D{{Key: "$cond", Value: bson.A{
				bson.D{{Key: "$and", Value: bson.A{
					bson.D{{Key: "$ne", Value: bson.A{"$resolvedAt", nil}}},
					bson.D{{Key: "$gt", Value: bson.A{"$resolvedAt", "$openedAt"}}},
				}}},
				bson.D{{Key: "$divide", Value: bson.A{
					bson.D{{Key: "$subtract", Value: bson.A{"$resolvedAt", "$openedAt"}}},
					60000,
				}}},
				nil,
			}}}},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$site"},
			{Key: "openIncidents", Value: sumIf("status", "OPEN")},
			{Key: "mttrMin", Value: bson.D{{Key: "$avg", Value: "$mttrMin"}}},
		}}},
	}

	excursionRows := bson.A{
		bson.D{{Key: "$match", Value: bson.M{
			"ts":    bson.M{"$gte": windowStart},
			"tempC": bson.M{"$gt": -14},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$meta.deviceId"},
			{Key: "excursionMin", Value: bson.D{{Key: "$sum", Value: 5.0 / 60.0}}}, // 5s cadence
		}}},
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "devices"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "deviceId"},
			{Key: "as", Value: "dev"},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "site", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$dev.site", 0}}}},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$site"},
			{Key: "avgExcursionMin", Value: bson.D{{Key: "$avg", Value: "$excursionMin"}}},
		}}},
	}

	// statusRows runs against devices; trend+excursion run against telemetry
	// (window + excursion math need ts/tempC), incidents against ai_incidents.
	pipeline := bson.A{
		bson.D{{Key: "$facet", Value: bson.D{
			{Key: "statusRows", Value: statusRows},
		}}},
	}

	cur, err := s.Devices.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	if !cur.Next(ctx) {
		return []SiteInsight{}, nil
	}
	var doc bson.M
	if err := cur.Decode(&doc); err != nil {
		return nil, err
	}
	// $facet emits ONE result doc whose fields are named after the branches
	// (the pipeline itself has no "facet" key) — e.g. {"statusRows": [...]}.
	facet := doc

	// telemetry branches: trend + excursions in one facet over `telemetry`
	trendRowsP := bson.A{
		bson.D{{Key: "$facet", Value: bson.D{
			{Key: "trendRows", Value: trendRows},
			{Key: "excursionRows", Value: excursionRows},
		}}},
	}
	tcur, err := s.db.Collection("telemetry").Aggregate(ctx, trendRowsP)
	if err != nil {
		return nil, err
	}
	defer tcur.Close(ctx)
	if tcur.Next(ctx) {
		var tdoc bson.M
		if err := tcur.Decode(&tdoc); err != nil {
			return nil, err
		}
		facet["trendRows"] = tdoc["trendRows"]
		facet["excursionRows"] = tdoc["excursionRows"]
	}

	// incidents branch over ai_incidents
	incidentRowsP := bson.A{
		bson.D{{Key: "$facet", Value: bson.D{
			{Key: "incidentRows", Value: incidentRows},
		}}},
	}
	icur, err := s.db.Collection("ai_incidents").Aggregate(ctx, incidentRowsP)
	if err != nil {
		return nil, err
	}
	defer icur.Close(ctx)
	if icur.Next(ctx) {
		var idoc bson.M
		if err := icur.Decode(&idoc); err != nil {
			return nil, err
		}
		facet["incidentRows"] = idoc["incidentRows"]
	}

	trend := map[string]float64{}
	for _, t := range asRows(facet["trendRows"]) {
		if id, ok := t["_id"].(string); ok {
			trend[id] = asFloat(t["maxSlopeCpm"])
		}
	}
	openInc := map[string]int{}
	mttrSum := map[string]float64{}
	mttrN := map[string]int{}
	for _, r := range asRows(facet["incidentRows"]) {
		id, _ := r["_id"].(string)
		openInc[id] = int(asFloat(r["openIncidents"]))
		if m, ok := r["mttrMin"]; ok && m != nil {
			if f, ok := m.(float64); ok {
				mttrSum[id] += f
				mttrN[id]++
			}
		}
	}
	exc := map[string]float64{}
	for _, e := range asRows(facet["excursionRows"]) {
		if id, ok := e["_id"].(string); ok {
			exc[id] = asFloat(e["avgExcursionMin"])
		}
	}

	var out []SiteInsight
	for _, r := range asRows(facet["statusRows"]) {
		id, _ := r["_id"].(string)
		row := SiteInsight{
			Site:            id,
			Devices:         int(asFloat(r["devices"])),
			OK:              int(asFloat(r["ok"])),
			AtRisk:          int(asFloat(r["atRisk"])),
			Alarm:           int(asFloat(r["alarm"])),
			ValueAtRisk:     int64(asFloat(r["valueAtRisk"])),
			MaxSlopeCPM:     trend[id],
			OpenIncidents:   openInc[id],
			AvgExcursionMin: exc[id],
		}
		if mttrN[id] > 0 {
			row.MTTRMin = mttrSum[id] / float64(mttrN[id])
		}
		out = append(out, row)
	}
	sortInsights(out)
	return out, nil
}

// asRows coerces a facet branch value into []bson.M (handles bson.M,
// map[string]any, and bson.D — the v2 driver decodes nested docs inside
// bson.A as ordered bson.D, which must be converted explicitly).
func asRows(v any) []bson.M {
	switch arr := v.(type) {
	case bson.A:
		out := make([]bson.M, 0, len(arr))
		for _, item := range arr {
			out = append(out, asRow(item))
		}
		return out
	}
	return nil
}

// asRow coerces one branch element into bson.M.
func asRow(item any) bson.M {
	switch m := item.(type) {
	case bson.M:
		return m
	case map[string]any:
		return bson.M(m)
	case bson.D:
		out := bson.M{}
		for _, e := range m {
			out[e.Key] = e.Value
		}
		return out
	case *bson.D:
		if m == nil {
			return nil
		}
		out := bson.M{}
		for _, e := range *m {
			out[e.Key] = e.Value
		}
		return out
	}
	return nil
}

// asFloat coerces an int32/int64/float64/decimal value to float64.
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

// sumIf — {$sum: {$cond: [{$eq:[$field, val]}, 1, 0]}} helper.
func sumIf(field string, val any) bson.D {
	return bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{
		bson.D{{Key: "$eq", Value: bson.A{"$" + field, val}}}, 1, 0,
	}}}}}
}

// sortInsights keeps site order stable (by site name).
func sortInsights(rows []SiteInsight) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Site < rows[j-1].Site; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
