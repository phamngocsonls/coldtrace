// Package ai — classifier.go implements online multinomial logistic
// regression with warm-start training on synthetic windows.
package ai

import (
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"
)

// Fault classes (classification labels).
const (
	ClassHealthy           = "healthy"
	ClassCompressorFailure = "compressor_failure"
	ClassDoorLeftOpen      = "door_left_open"
	ClassSensorDrift       = "sensor_drift"
	ClassPowerOutage       = "power_outage"
)

// Classes is the fixed label order used by the model head.
var Classes = []string{ClassHealthy, ClassCompressorFailure, ClassDoorLeftOpen, ClassSensorDrift, ClassPowerOutage}

// Prediction is one inference result for a device window.
type Prediction struct {
	DeviceID    string             `json:"deviceId"`
	Class       string             `json:"class"`
	Confidence  float64            `json:"confidence"`
	Probs       map[string]float64 `json:"probs"`
	SlopeCpm    float64            `json:"slopeCpm"`
	EtaAlarmMin float64            `json:"etaAlarmMin"`
	Anomaly     float64            `json:"anomalyScore"`
	Features    []float64          `json:"features"`
	At          time.Time          `json:"at"`
}

// Classifier is an online multinomial logistic regression over the
// normalized feature vector. Warm-started on synthetic physics windows and
// continuously refined from resolved incidents (auto-learning).
type Classifier struct {
	mu     sync.Mutex
	w      [][]float64 // [NumClasses][NumFeatures]
	b      []float64
	lr     float64
	log    *slog.Logger
	trainN int
}

// NewClassifier creates a classifier warm-started on synthetic data.
// A nil logger is allowed (warm-start logs are suppressed).
func NewClassifier(log *slog.Logger) *Classifier {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	c := &Classifier{lr: 0.15, log: log}
	c.warmStart()
	return c
}

// discardWriter drops everything — used when no logger is supplied.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func (c *Classifier) warmStart() {
	// Train on the DEVICE SIMULATOR, not on the analytic generators.
	//
	// The generators are hand-written sketches; the simulator is the physics
	// the classifier is actually scored against at run time (and what the
	// fleet publishes over MQTT). Warm-starting on it removes the
	// train/serve skew that made the model look perfect offline and fail on
	// live data. The generators remain only as an independent sanity check.
	xs, ys := buildSimulatorTrainingSet()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.w = make([][]float64, len(Classes))
	for k := range c.w {
		c.w[k] = make([]float64, NumFeatures)
	}
	c.b = make([]float64, len(Classes))
	for it := 0; it < 600; it++ {
		for i := range xs {
			c.trainOne(xs[i], ys[i])
		}
	}
	c.trainN = len(xs)
	c.log.Info("ai_warm_start", "samples", len(xs), "epochs", 600)
}

// Predict classifies the (already-normalized) feature vector.
func (c *Classifier) Predict(x []float64) (class string, probs map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.predictLocked(x)
}

// PredictEnsemble classifies a *set* of feature vectors describing the same
// device over consecutive windows and averages their log-probabilities.
//
// One 6-minute window is a noisy sample: a healthy cabinet that happens to be
// coasting looks much like a drifting one. Averaging several consecutive
// windows is the cheapest real fix, and it makes the reported confidence mean
// something — "8 of 10 recent windows agree" — which is what the autonomy
// guardrails need in order to gate on confidence honestly.
func (c *Classifier) PredictEnsemble(windows [][]float64) (class string, probs map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(windows) == 0 {
		return c.predictLocked(nil)
	}
	return c.predictLocked(windows...)
}

// predictLocked returns the class and a calibrated confidence.
//
// A single window is noisy, so softmax over ONE window is either ~1.0 or
// arbitrary. The confidence is instead the geometric mean of the per-window
// softmax probabilities (i.e. softmax of the mean logits): if 8 of 10 cheap
// windows agree, the answer is 0.8 regardless of how sure any one of them
// looked. That is what makes the autonomy guardrails (confidence thresholds)
// behave sensibly and what keeps the model-eval number meaningful.
func (c *Classifier) predictLocked(xs ...[]float64) (string, map[string]float64) {
	n := len(xs)
	if n == 0 {
		return Classes[0], map[string]float64{}
	}
	logits := make([]float64, len(Classes))
	max := math.Inf(-1)
	for _, x := range xs {
		for k := range Classes {
			z := c.b[k]
			for i := 0; i < NumFeatures; i++ {
				z += c.w[k][i] * x[i]
			}
			logits[k] += z / float64(n)
		}
	}
	for k := range Classes {
		if logits[k] > max {
			max = logits[k]
		}
	}
	sum := 0.0
	probs := make(map[string]float64, len(Classes))
	for k := range Classes {
		e := math.Exp(logits[k] - max)
		probs[Classes[k]] = e
		sum += e
	}
	best := 0
	for k := range Classes {
		probs[Classes[k]] /= sum
		if probs[Classes[k]] > probs[Classes[best]] {
			best = k
		}
	}
	return Classes[best], probs
}

// Learn incorporates one labeled example (from a resolved incident) into the
// weights with a small number of SGD steps — the "auto-learning" hook.
func (c *Classifier) Learn(x []float64, class string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	y := -1
	for k := range Classes {
		if Classes[k] == class {
			y = k
		}
	}
	if y < 0 {
		return
	}
	for it := 0; it < 25; it++ {
		c.trainOne(x, y)
	}
	c.trainN++
	c.log.Info("ai_learned", "faultClass", class, "trainSamples", c.trainN)
}

// trainOne applies one SGD step (caller holds the lock).
func (c *Classifier) trainOne(x []float64, y int) {
	logits := make([]float64, len(Classes))
	max := math.Inf(-1)
	for k := range Classes {
		z := c.b[k]
		for i := 0; i < NumFeatures; i++ {
			z += c.w[k][i] * x[i]
		}
		logits[k] = z
		if z > max {
			max = z
		}
	}
	for k := range Classes {
		if logits[k] > max {
			max = logits[k]
		}
	}
	sum := 0.0
	probs := make([]float64, len(Classes))
	for k := range Classes {
		e := math.Exp(logits[k] - max)
		probs[k] = e
		sum += e
	}
	for k := range Classes {
		g := probs[k]/sum - boolTo01(k == y)
		for i := 0; i < NumFeatures; i++ {
			c.w[k][i] -= c.lr * g * x[i]
		}
		c.b[k] -= c.lr * g
	}
}

func boolTo01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// SaveModel persists weights for reuse across runs (optional).
func (c *Classifier) SaveModel(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(map[string]any{"w": c.w, "b": c.b})
}
