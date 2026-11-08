// Package ai — normalize.go holds the fixed feature scaler shared by
// warm-start training and live inference (z-score standardization).
package ai

// normMean/normStd are fitted to the pooled feature distribution produced by
// the device simulator (internal/sim) across all five classes — see
// TestScalerFitsSimulator. Keeping one scaler for training and inference is
// what makes the warm-started weights meaningful at runtime; hand-picked
// constants silently rescale every feature and the model degrades without any
// error surfacing.
var normMean = [NumFeatures]float64{
	-17.3176, // temp_mean
	0.2979,   // temp_std
	0.1056,   // slope_cpm
	0.3604,   // compressor_duty
	240.0000, // power_mean
	14.7207,  // door_events
	1.0381,   // temp_range
	0.0569,   // slope_accel
	0.4940,   // temp_offset
	0.1156,   // drift_lag_cpm
	0.8691,   // compressor_reattain
	0.1125,   // trend_lag_cpm
	0.1110,   // net_trend_cpm
}

var normStd = [NumFeatures]float64{
	0.7226,   // temp_mean
	0.1977,   // temp_std
	0.1587,   // slope_cpm
	0.4020,   // compressor_duty
	195.9592, // power_mean
	28.8303,  // door_events
	0.6196,   // temp_range
	0.1950,   // slope_accel
	0.2860,   // temp_offset
	0.1413,   // drift_lag_cpm
	0.2013,   // compressor_reattain
	0.1654,   // trend_lag_cpm
	0.1631,   // net_trend_cpm
}

// Normalize z-scores a raw feature vector.
func Normalize(x []float64) []float64 {
	out := make([]float64, len(x))
	for i, v := range x {
		out[i] = (v - normMean[i]) / normStd[i]
	}
	return out
}
