// Package diarize implements the meeting-diarization service: batch speaker diarization,
// speaker identification against enrolled voice profiles, and word-level transcript
// attribution, served over an HTTP contract wire-compatible with the Python
// meeting-diarizer (multipart POST /transcribe on its own port).
//
// The pipeline and its tuning constants are a port of meeting-diarizer/app/diarizer.py;
// changes to thresholds or matching logic should stay in sync with the rationale
// documented there.
package diarize

// Constants ported from meeting-diarizer/app/diarizer.py — see that file for the
// empirical history behind each value.
const (
	// SimilarityThreshold is the default identification cosine cutoff. Raised from 0.45
	// after all profiles were rebuilt from current-hardware audio (profiles now score
	// 0.76-0.99 against their own voice; the impostor ceiling observed was 0.46).
	SimilarityThreshold = 0.70
	// AttendeeOffset is subtracted from a speaker's score when an attendee list is given
	// and the speaker is not on it.
	AttendeeOffset = 0.15
	// AmbiguousMargin flags a cluster whose top two scores are closer than this.
	AmbiguousMargin = 0.05
	// MinSegmentSec is the shortest diarized segment used for cluster embeddings.
	MinSegmentSec = 0.5
	// MaxEmbedSegments caps how many (longest-first) segments feed a cluster embedding.
	MaxEmbedSegments = 30
	// EnrollCandidatePct: unidentified speakers with at least this share of speech are
	// suggested as enrollment candidates.
	EnrollCandidatePct = 5.0
	// ClusterMergeThreshold: sherpa's FastClustering over-splits speakers badly compared
	// to the pyannote pipeline (62 clusters where pyannote found 4, on a real meeting).
	// After sherpa runs, clusters whose long-segment averaged embeddings score at least
	// this cosine are merged. In the eres2net space genuine same-speaker comparisons
	// measured >= 0.70 (enrollment-quality audio) and impostors <= 0.54, so 0.60 splits
	// the difference with margin both ways.
	ClusterMergeThreshold = 0.60
	// ChannelMeRatio: when a caller marks one channel as an isolated microphone (open-quake
	// records the user's mic on the left channel, everyone else's loopback on the right), a
	// cluster whose speech energy on the mic channel is at least this many times its energy on
	// the other channel is that user — ground truth, no cosine threshold. During the user's own
	// speech the mic channel dominates hugely; during everyone else's it's near silent (only
	// bleed), so the two populations separate with wide margin and 3x is safely clear of both.
	ChannelMeRatio = 3.0

	// MinClusterSec: after merging, clusters still shorter than this are micro-fragments
	// of crosstalk and backchannel that sherpa split off but whose embeddings are too
	// weak to merge anywhere (typically one 1-2s segment). They are dropped from the
	// timeline — their words fall to UNKNOWN and the same-speaker-both-sides fill gives
	// them to whoever is talking around them, the same treatment the pipeline already
	// gives turn-edge slivers. On the reference meeting this took 41 reported speakers
	// (37 of them holding 8% of the speech) down to pyannote's 4.
	MinClusterSec = 5.0
)

// ThresholdSweep are the retrospective what-if thresholds included in every report.
var ThresholdSweep = []float64{0.30, 0.40, 0.50, 0.55, 0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90, 0.95}
