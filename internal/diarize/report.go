package diarize

import "math"

// The report structs mirror meeting-diarizer/app/diarizer.py's dicts field-for-field —
// struct order IS the JSON key order, and the downstream transcript-cleanup tooling
// consumes this schema unchanged. Nullable fields use pointers so JSON says null
// exactly where the Python service does; empty lists must marshal as [] (callers
// initialize the slices), never null.

type Score struct {
	Name     string  `json:"name"`
	Raw      float64 `json:"raw"`   // cosine similarity, 4dp
	Score    float64 `json:"score"` // raw minus any attendee offset, 4dp
	Attendee *bool   `json:"attendee"`
}

type ClusterReport struct {
	Cluster       string       `json:"cluster"`
	SegmentCount  int          `json:"segment_count"`
	DurationSec   float64      `json:"duration_sec"`
	SegmentsUsed  int          `json:"segments_used"`
	SecondsUsed   float64      `json:"seconds_used"`
	EmbedSegments [][2]float64 `json:"embed_segments"`
	Matched       *string      `json:"matched"`
	Margin        *float64     `json:"margin"`
	Ambiguous     bool         `json:"ambiguous"`
	Scores        []Score      `json:"scores"`
	// ChannelMatched is set when this cluster was named from an isolated-microphone
	// channel rather than voice matching (see MeHint). Omitted otherwise, so the
	// report schema is unchanged for recordings that don't use the feature.
	ChannelMatched bool `json:"channel_matched,omitempty"`
}

type SpeakerSummary struct {
	Label       string   `json:"label"`
	Identified  bool     `json:"identified"`
	DurationSec float64  `json:"duration_sec"`
	Clusters    int      `json:"clusters"`
	BestScore   *float64 `json:"best_score"`
	Nearest     *string  `json:"nearest"`
	SpeechPct   float64  `json:"speech_pct"`
}

type EnrollmentCandidate struct {
	Label       string   `json:"label"`
	DurationSec float64  `json:"duration_sec"`
	SpeechPct   float64  `json:"speech_pct"`
	Nearest     *string  `json:"nearest"`
	BestScore   *float64 `json:"best_score"`
}

type SweepRow struct {
	Threshold       float64 `json:"threshold"`
	ClustersMatched int     `json:"clusters_matched"`
	DistinctNames   int     `json:"distinct_names"`
	Collisions      int     `json:"collisions"`
	SpeechNamedPct  float64 `json:"speech_named_pct"`
}

type SpeakerReport struct {
	ThresholdUsed        float64               `json:"threshold_used"`
	AttendeesApplied     []string              `json:"attendees_applied"` // sorted, or null
	SpeakerCount         int                   `json:"speaker_count"`
	TotalSpeechSec       float64               `json:"total_speech_sec"`
	SpeechNamedPct       float64               `json:"speech_named_pct"`
	Speakers             []SpeakerSummary      `json:"speakers"`
	EnrollmentCandidates []EnrollmentCandidate `json:"enrollment_candidates"`
	Clusters             []ClusterReport       `json:"clusters"`
	ThresholdSweep       []SweepRow            `json:"threshold_sweep"`
}

// Segment is one attributed span of transcript text.
type Segment struct {
	Speaker string  `json:"speaker"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// thresholdSweep evaluates every candidate threshold against the already-computed
// (and already-rounded) cluster scores, matching _threshold_sweep in diarizer.py.
func thresholdSweep(clusters []ClusterReport) []SweepRow {
	total := 0.0
	for _, c := range clusters {
		total += c.DurationSec
	}
	if total == 0 {
		total = 1.0
	}
	sweep := make([]SweepRow, 0, len(ThresholdSweep))
	for _, t := range ThresholdSweep {
		named := 0.0
		names := map[string]bool{}
		hits := 0
		for _, c := range clusters {
			if len(c.Scores) > 0 && c.Scores[0].Score >= t {
				hits++
				named += c.DurationSec
				names[c.Scores[0].Name] = true
			}
		}
		sweep = append(sweep, SweepRow{
			Threshold:       t,
			ClustersMatched: hits,
			DistinctNames:   len(names),
			Collisions:      hits - len(names),
			SpeechNamedPct:  round1(named / total * 100),
		})
	}
	return sweep
}

// summariseSpeakers rolls clusters up into one entry per display name, matching
// _summarise_speakers in diarizer.py (merged clusters flag pyannote splitting one
// person in two; the sweep's collision count surfaces it).
func summariseSpeakers(clusters []ClusterReport) []SpeakerSummary {
	total := 0.0
	for _, c := range clusters {
		total += c.DurationSec
	}
	if total == 0 {
		total = 1.0
	}
	var order []string
	agg := map[string]*SpeakerSummary{}
	for _, c := range clusters {
		a, ok := agg[c.Cluster]
		if !ok {
			a = &SpeakerSummary{Label: c.Cluster, Identified: c.Matched != nil}
			agg[c.Cluster] = a
			order = append(order, c.Cluster)
		}
		a.DurationSec += c.DurationSec
		a.Clusters++
		if len(c.Scores) > 0 {
			top := c.Scores[0]
			if a.BestScore == nil || top.Score > *a.BestScore {
				s, n := top.Score, top.Name
				a.BestScore, a.Nearest = &s, &n
			}
		}
	}
	out := make([]SpeakerSummary, 0, len(order))
	for _, name := range order {
		a := agg[name]
		a.DurationSec = round2(a.DurationSec)
		a.SpeechPct = round1(a.DurationSec / total * 100)
		out = append(out, *a)
	}
	// Stable sort by duration descending, ties keeping first-appearance order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].DurationSec > out[j-1].DurationSec; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// buildReport assembles the full speaker report, matching _build_report in diarizer.py.
func buildReport(clusters []ClusterReport, threshold float64, attendees []string) *SpeakerReport {
	total := 0.0
	for _, c := range clusters {
		total += c.DurationSec
	}
	speakers := summariseSpeakers(clusters)
	named := 0.0
	for _, s := range speakers {
		if s.Identified {
			named += s.DurationSec
		}
	}
	candidates := make([]EnrollmentCandidate, 0)
	for _, s := range speakers {
		if !s.Identified && s.SpeechPct >= EnrollCandidatePct {
			candidates = append(candidates, EnrollmentCandidate{
				Label: s.Label, DurationSec: s.DurationSec, SpeechPct: s.SpeechPct,
				Nearest: s.Nearest, BestScore: s.BestScore,
			})
		}
	}
	namedPct := 0.0
	if total > 0 {
		namedPct = round1(named / total * 100)
	}
	return &SpeakerReport{
		ThresholdUsed:        threshold,
		AttendeesApplied:     attendees,
		SpeakerCount:         len(speakers),
		TotalSpeechSec:       round2(total),
		SpeechNamedPct:       namedPct,
		Speakers:             speakers,
		EnrollmentCandidates: candidates,
		Clusters:             clusters,
		ThresholdSweep:       thresholdSweep(clusters),
	}
}
