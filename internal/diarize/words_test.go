package diarize

import (
	"reflect"
	"testing"
)

func w(text string, start, end float64) Word { return Word{Text: text, Start: start, End: end} }

func TestAttributeWordsMidpointAndFill(t *testing.T) {
	timeline := []Turn{
		{Start: 0.0, End: 2.0, Label: 0},
		{Start: 2.5, End: 5.0, Label: 1},
	}
	labels := map[int]string{0: "Alice", 1: "Speaker A"}
	words := []Word{
		w(" hello", 0.1, 0.5),  // Alice
		w(" there", 0.6, 1.0),  // Alice
		w(" um", 2.1, 2.3),     // midpoint 2.2 in the gap -> UNKNOWN at a speaker change: stays
		w(" yes", 2.6, 3.0),    // Speaker A
		w(" indeed", 3.1, 3.5), // Speaker A
	}
	got := attributeWords(words, timeline, labels)
	want := []Segment{
		{Speaker: "Alice", Start: 0.1, End: 1.0, Text: "hello there"},
		{Speaker: "UNKNOWN", Start: 2.1, End: 2.3, Text: "um"},
		{Speaker: "Speaker A", Start: 2.6, End: 3.5, Text: "yes indeed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestAttributeWordsFillsSameSpeakerRun(t *testing.T) {
	timeline := []Turn{
		{Start: 0.0, End: 1.0, Label: 0},
		{Start: 1.4, End: 3.0, Label: 0}, // same speaker resumes after a sliver gap
	}
	labels := map[int]string{0: "Bob"}
	words := []Word{
		w(" one", 0.1, 0.5),
		w(" two", 1.1, 1.3), // midpoint 1.2 in the sliver -> filled with Bob
		w(" three", 1.5, 2.0),
	}
	got := attributeWords(words, timeline, labels)
	want := []Segment{{Speaker: "Bob", Start: 0.1, End: 2.0, Text: "one two three"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestAttributeWordsLeadingUnknownStays(t *testing.T) {
	timeline := []Turn{{Start: 1.0, End: 2.0, Label: 0}}
	labels := map[int]string{0: "Ann"}
	words := []Word{
		w(" pre", 0.0, 0.4), // before any turn, nothing on the left -> UNKNOWN
		w(" hi", 1.2, 1.6),
	}
	got := attributeWords(words, timeline, labels)
	want := []Segment{
		{Speaker: "UNKNOWN", Start: 0.0, End: 0.4, Text: "pre"},
		{Speaker: "Ann", Start: 1.2, End: 1.6, Text: "hi"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Schmitz, TJ":      "tjschmitz",
		"Schmitz, T.J.":    "tjschmitz",
		"T.J. Schmitz":     "tjschmitz",
		"Matthew Evenson":  "matthewevenson",
		"Evenson, Matthew": "matthewevenson",
		"  José  ":         "jos", // non-ascii folds away, like the Python regex
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSweepAndSummarise(t *testing.T) {
	name := "Alice"
	m := 0.2
	att := false
	clusters := []ClusterReport{
		{
			Cluster: "Alice", DurationSec: 60.0, SegmentCount: 5,
			Matched: &name, Margin: &m,
			Scores:        []Score{{Name: "Alice", Raw: 0.85, Score: 0.85}, {Name: "Bob", Raw: 0.65, Score: 0.65}},
			EmbedSegments: [][2]float64{},
		},
		{
			Cluster: "Alice", DurationSec: 20.0, SegmentCount: 2, // pyannote split her
			Matched:       &name,
			Scores:        []Score{{Name: "Alice", Raw: 0.75, Score: 0.75}},
			EmbedSegments: [][2]float64{},
		},
		{
			Cluster: "Speaker A", DurationSec: 20.0, SegmentCount: 3,
			Scores:        []Score{{Name: "Bob", Raw: 0.40, Score: 0.25, Attendee: &att}},
			EmbedSegments: [][2]float64{},
		},
	}
	speakers := summariseSpeakers(clusters)
	if len(speakers) != 2 {
		t.Fatalf("speakers = %+v", speakers)
	}
	if speakers[0].Label != "Alice" || speakers[0].Clusters != 2 || speakers[0].DurationSec != 80.0 ||
		*speakers[0].BestScore != 0.85 || speakers[0].SpeechPct != 80.0 {
		t.Errorf("Alice summary = %+v", speakers[0])
	}
	if speakers[1].Label != "Speaker A" || speakers[1].Identified || *speakers[1].Nearest != "Bob" {
		t.Errorf("Speaker A summary = %+v", speakers[1])
	}

	report := buildReport(clusters, 0.70, nil)
	if report.SpeakerCount != 2 || report.TotalSpeechSec != 100.0 || report.SpeechNamedPct != 80.0 {
		t.Errorf("report = %+v", report)
	}
	// Speaker A holds 20% >= 5% -> enrollment candidate
	if len(report.EnrollmentCandidates) != 1 || report.EnrollmentCandidates[0].Label != "Speaker A" {
		t.Errorf("candidates = %+v", report.EnrollmentCandidates)
	}
	// Sweep at 0.70: both Alice clusters matched at 0.85/0.75 -> 2 hits, 1 name, 1 collision, 80%
	for _, row := range report.ThresholdSweep {
		if row.Threshold == 0.70 {
			if row.ClustersMatched != 2 || row.DistinctNames != 1 || row.Collisions != 1 || row.SpeechNamedPct != 80.0 {
				t.Errorf("sweep@0.70 = %+v", row)
			}
		}
		if row.Threshold == 0.80 {
			if row.ClustersMatched != 1 || row.Collisions != 0 || row.SpeechNamedPct != 60.0 {
				t.Errorf("sweep@0.80 = %+v", row)
			}
		}
	}
}
