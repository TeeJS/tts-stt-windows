package diarize

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// Diarizer wraps sherpa-onnx speaker diarization plus identification against the
// enrollment store. Methods are not safe for concurrent use — the HTTP layer
// serializes requests.
type Diarizer struct {
	sd    *sherpa.OfflineSpeakerDiarization
	ex    *sherpa.SpeakerEmbeddingExtractor
	store *EnrollmentStore
}

// Turn is one diarized span: speaker Label is sherpa's integer cluster id.
type Turn struct {
	Start, End float64
	Label      int
}

func NewDiarizer(segModel, embedModel string, threads int, clusterThreshold float32, store *EnrollmentStore) (*Diarizer, error) {
	sd := sherpa.NewOfflineSpeakerDiarization(&sherpa.OfflineSpeakerDiarizationConfig{
		Segmentation: sherpa.OfflineSpeakerSegmentationModelConfig{
			Pyannote:   sherpa.OfflineSpeakerSegmentationPyannoteModelConfig{Model: segModel},
			NumThreads: threads,
			Provider:   "cpu",
		},
		Embedding: sherpa.SpeakerEmbeddingExtractorConfig{
			Model:      embedModel,
			NumThreads: threads,
			Provider:   "cpu",
		},
		Clustering:     sherpa.FastClusteringConfig{NumClusters: -1, Threshold: clusterThreshold},
		MinDurationOn:  0.3,
		MinDurationOff: 0.5,
	})
	if sd == nil {
		return nil, fmt.Errorf("failed to load diarization models (%s, %s)", segModel, embedModel)
	}
	ex := sherpa.NewSpeakerEmbeddingExtractor(&sherpa.SpeakerEmbeddingExtractorConfig{
		Model: embedModel, NumThreads: threads, Provider: "cpu",
	})
	if ex == nil {
		sherpa.DeleteOfflineSpeakerDiarization(sd)
		return nil, fmt.Errorf("failed to load speaker-embedding model %s", embedModel)
	}
	return &Diarizer{sd: sd, ex: ex, store: store}, nil
}

func (d *Diarizer) Close() {
	if d.sd != nil {
		sherpa.DeleteOfflineSpeakerDiarization(d.sd)
		d.sd = nil
	}
	if d.ex != nil {
		sherpa.DeleteSpeakerEmbeddingExtractor(d.ex)
		d.ex = nil
	}
}

// Timeline diarizes the whole recording in one pass (16 kHz mono samples).
func (d *Diarizer) Timeline(samples []float32) []Turn {
	if len(samples) == 0 {
		return nil
	}
	segs := d.sd.Process(samples)
	turns := make([]Turn, 0, len(segs))
	for _, s := range segs {
		turns = append(turns, Turn{Start: float64(s.Start), End: float64(s.End), Label: s.Speaker})
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i].Start < turns[j].Start })
	return turns
}

// embed computes one embedding for a span of the recording.
func (d *Diarizer) embed(samples []float32, start, end float64) []float32 {
	lo := int(start * pipelineRate)
	hi := int(end * pipelineRate)
	if lo < 0 {
		lo = 0
	}
	if hi > len(samples) {
		hi = len(samples)
	}
	if hi <= lo {
		return nil
	}
	stream := d.ex.CreateStream()
	defer sherpa.DeleteOnlineStream(stream)
	stream.AcceptWaveform(pipelineRate, samples[lo:hi])
	stream.InputFinished()
	if !d.ex.IsReady(stream) {
		return nil
	}
	return d.ex.Compute(stream)
}

// clusterEmbedding ports _cluster_embedding: segments >= MinSegmentSec, longest-first,
// at most MaxEmbedSegments, each L2-normalized before averaging, the mean re-normalized.
// Returns nil if nothing usable. used lists the contributing spans (for re-enrollment).
func (d *Diarizer) clusterEmbedding(samples []float32, segs []Turn) ([]float64, [][2]float64) {
	usable := make([]Turn, 0, len(segs))
	for _, s := range segs {
		if s.End-s.Start >= MinSegmentSec {
			usable = append(usable, s)
		}
	}
	sort.SliceStable(usable, func(i, j int) bool {
		return usable[i].End-usable[i].Start > usable[j].End-usable[j].Start
	})
	if len(usable) > MaxEmbedSegments {
		usable = usable[:MaxEmbedSegments]
	}

	var sum []float64
	used := make([][2]float64, 0, len(usable))
	for _, seg := range usable {
		emb := d.embed(samples, seg.Start, seg.End)
		if emb == nil {
			continue
		}
		var norm float64
		nan := false
		for _, v := range emb {
			f := float64(v)
			if math.IsNaN(f) {
				nan = true
				break
			}
			norm += f * f
		}
		if nan {
			continue
		}
		norm = math.Sqrt(norm)
		if norm < 1e-8 {
			continue
		}
		if sum == nil {
			sum = make([]float64, len(emb))
		}
		for i, v := range emb {
			sum[i] += float64(v) / norm
		}
		used = append(used, [2]float64{seg.Start, seg.End})
	}
	if sum == nil {
		return nil, nil
	}
	var norm float64
	for i := range sum {
		sum[i] /= float64(len(used))
		norm += sum[i] * sum[i]
	}
	norm = math.Sqrt(norm) + 1e-8
	for i := range sum {
		sum[i] /= norm
	}
	return sum, used
}

// cosineSimilarity matches _cosine_similarity: dot / (|a|*|b| + 1e-8).
func cosineSimilarity(a []float64, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		bf := float64(b[i])
		dot += a[i] * bf
		na += a[i] * a[i]
		nb += bf * bf
	}
	return dot / (math.Sqrt(na)*math.Sqrt(nb) + 1e-8)
}

var nonAlnum = regexp.MustCompile(`[^0-9a-z]`)

// NormalizeName folds a display name to a comparison key: "Last, First" becomes
// "First Last", then lowercase alphanumerics only — so "Schmitz, T.J." and
// "T.J. Schmitz" agree. Diminutives are deliberately NOT folded (Matt != Matthew).
func NormalizeName(name string) string {
	n := strings.TrimSpace(name)
	if last, first, ok := strings.Cut(n, ","); ok {
		n = strings.TrimSpace(first) + " " + strings.TrimSpace(last)
	}
	return nonAlnum.ReplaceAllString(strings.ToLower(n), "")
}

// resolveAttendees maps requested attendee names onto enrolled speakers via
// NormalizeName. Returns nil when no attendee list was given.
func (d *Diarizer) resolveAttendees(attendees []string) map[string]bool {
	if len(attendees) == 0 {
		return nil
	}
	names, _ := d.store.ListSpeakers()
	byNorm := make(map[string]string, len(names))
	for _, e := range names {
		byNorm[NormalizeName(e)] = e
	}
	set := map[string]bool{}
	for _, req := range attendees {
		if enrolled, ok := byNorm[NormalizeName(req)]; ok {
			set[enrolled] = true
		}
	}
	return set
}

// identify ports _identify: cosine against every enrolled profile, attendee offset
// for names off the list, rounded scores sorted best-first; matched iff best >= threshold.
func (d *Diarizer) identify(emb []float64, threshold float64, attendees map[string]bool) (*string, []Score) {
	for _, v := range emb {
		if math.IsNaN(v) {
			return nil, []Score{}
		}
	}
	profiles, err := d.store.AllEmbeddings()
	if err != nil {
		return nil, []Score{}
	}
	names := make([]string, 0, len(profiles))
	for n := range profiles {
		names = append(names, n)
	}
	sort.Strings(names)

	scores := make([]Score, 0, len(names))
	for _, name := range names {
		raw := cosineSimilarity(emb, profiles[name])
		isAttendee := attendees == nil || attendees[name]
		score := raw
		if !isAttendee {
			score = raw - AttendeeOffset
		}
		s := Score{Name: name, Raw: round4(raw), Score: round4(score)}
		if attendees != nil {
			att := isAttendee
			s.Attendee = &att
		}
		scores = append(scores, s)
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].Score > scores[j].Score })

	if len(scores) > 0 && scores[0].Score >= threshold {
		name := scores[0].Name
		return &name, scores
	}
	return nil, scores
}

// MeHint marks one channel of the recording as a known speaker's isolated microphone,
// letting the pipeline name that speaker's cluster from channel energy instead of voice
// matching. Name is the speaker (resolved to their enrolled display name when it matches
// an enrollment); Channel is the mic channel's index; Signals are the per-channel 16 kHz
// signals from DecodeAudioChannels. Nil (or fewer than two channels) disables it.
type MeHint struct {
	Name    string
	Channel int
	Signals [][]float32
}

func (h *MeHint) active() bool {
	return h != nil && strings.TrimSpace(h.Name) != "" &&
		len(h.Signals) >= 2 && h.Channel >= 0 && h.Channel < len(h.Signals)
}

// channelEnergy sums the squared sample energy of a signal over a set of turns.
func channelEnergy(signal []float32, segs []Turn) float64 {
	var e float64
	for _, s := range segs {
		lo, hi := int(s.Start*pipelineRate), int(s.End*pipelineRate)
		if lo < 0 {
			lo = 0
		}
		if hi > len(signal) {
			hi = len(signal)
		}
		for i := lo; i < hi; i++ {
			e += float64(signal[i]) * float64(signal[i])
		}
	}
	return e
}

// isMe reports whether a cluster's speech is dominated by the mic channel — i.e. it is
// the hinted user talking, not their mic picking up someone else's bleed.
func (h *MeHint) isMe(segs []Turn) bool {
	if !h.active() {
		return false
	}
	me := channelEnergy(h.Signals[h.Channel], segs)
	var other float64
	for i, sig := range h.Signals {
		if i != h.Channel {
			other += channelEnergy(sig, segs)
		}
	}
	return me > 1e-6 && me >= other*ChannelMeRatio
}

// protoCluster is a speaker cluster mid-analysis: sherpa's raw output grouped by
// label, possibly merged with other clusters before identification.
type protoCluster struct {
	labels   []int // sherpa labels folded into this cluster
	segs     []Turn
	duration float64
	emb      []float64 // normalized long-segment embedding, nil if nothing usable
	used     [][2]float64
}

func cos64(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	return dot / (math.Sqrt(na)*math.Sqrt(nb) + 1e-8)
}

// mergeClusters greedily merges the closest pair of clusters (average-linkage on
// duration-weighted embedding means) until no pair scores >= ClusterMergeThreshold.
// This compensates for sherpa's FastClustering over-splitting: our per-cluster
// embeddings average the longest segments, which is far more stable than the short
// windows sherpa clusters internally.
func (d *Diarizer) mergeClusters(samples []float32, clusters []protoCluster) []protoCluster {
	for {
		bi, bj, best := -1, -1, ClusterMergeThreshold
		for i := 0; i < len(clusters); i++ {
			if clusters[i].emb == nil {
				continue
			}
			for j := i + 1; j < len(clusters); j++ {
				if clusters[j].emb == nil {
					continue
				}
				if c := cos64(clusters[i].emb, clusters[j].emb); c >= best {
					bi, bj, best = i, j, c
				}
			}
		}
		if bi < 0 {
			break
		}
		a, b := &clusters[bi], clusters[bj]
		merged := make([]float64, len(a.emb))
		var norm float64
		for k := range merged {
			merged[k] = a.emb[k]*a.duration + b.emb[k]*b.duration
			norm += merged[k] * merged[k]
		}
		norm = math.Sqrt(norm) + 1e-8
		for k := range merged {
			merged[k] /= norm
		}
		a.labels = append(a.labels, b.labels...)
		a.segs = append(a.segs, b.segs...)
		a.duration += b.duration
		a.emb = merged
		clusters = append(clusters[:bj], clusters[bj+1:]...)
	}
	// Re-derive each merged cluster's embedding from the union of its segments so
	// identification sees the same longest-first selection a single cluster would get.
	for i := range clusters {
		if len(clusters[i].labels) > 1 {
			clusters[i].emb, clusters[i].used = d.clusterEmbedding(samples, clusters[i].segs)
		}
	}
	return clusters
}

// analyzeAndLabel ports _analyze_clusters + the relettering in _diarize, with the
// sherpa-specific merge pass in between: one report entry per (merged) cluster;
// identified clusters take the matched name, the rest become "Speaker A",
// "Speaker B", ... in order of appearance. Returns the cluster reports and a map
// from sherpa's integer label to the display name.
func (d *Diarizer) analyzeAndLabel(samples []float32, timeline []Turn, threshold float64, attendees map[string]bool, me *MeHint) ([]ClusterReport, map[int]string) {
	labels := map[int]bool{}
	for _, t := range timeline {
		labels[t.Label] = true
	}
	unique := make([]int, 0, len(labels))
	for l := range labels {
		unique = append(unique, l)
	}
	sort.Ints(unique)

	enrolled, _ := d.store.ListSpeakers()
	haveEnrollments := len(enrolled) > 0

	protos := make([]protoCluster, 0, len(unique))
	for _, label := range unique {
		p := protoCluster{labels: []int{label}}
		for _, t := range timeline {
			if t.Label == label {
				p.segs = append(p.segs, t)
				p.duration += t.End - t.Start
			}
		}
		p.emb, p.used = d.clusterEmbedding(samples, p.segs)
		protos = append(protos, p)
	}
	protos = d.mergeClusters(samples, protos)

	// Drop unmergeable micro-fragments; their speech is attributed via the
	// UNKNOWN-fill word logic instead (see MinClusterSec).
	kept := protos[:0]
	for _, p := range protos {
		if p.duration >= MinClusterSec {
			kept = append(kept, p)
		}
	}
	protos = kept

	meLabel := ""
	if me.active() {
		meLabel = d.resolveMeName(me.Name)
	}

	clusters := make([]ClusterReport, 0, len(protos))
	labelMap := make(map[int]string, len(unique))
	unnamed := 0
	for _, p := range protos {
		entry := ClusterReport{
			SegmentCount:  len(p.segs),
			DurationSec:   round2(p.duration),
			EmbedSegments: [][2]float64{},
			Scores:        []Score{},
		}
		if haveEnrollments {
			secUsed := 0.0
			for _, u := range p.used {
				secUsed += u[1] - u[0]
			}
			entry.SegmentsUsed = len(p.used)
			entry.SecondsUsed = round2(secUsed)
			for _, u := range p.used {
				entry.EmbedSegments = append(entry.EmbedSegments, [2]float64{round2(u[0]), round2(u[1])})
			}
			if p.emb != nil {
				entry.Matched, entry.Scores = d.identify(p.emb, threshold, attendees)
				if len(entry.Scores) > 1 {
					m := round4(entry.Scores[0].Score - entry.Scores[1].Score)
					entry.Margin = &m
					entry.Ambiguous = m < AmbiguousMargin
				}
			}
		}
		// An isolated-mic channel is ground truth: if this cluster's speech is
		// mic-channel-dominant it IS the hinted user, overriding whatever the
		// embedding said (the cosine scores stay in the report for transparency).
		switch {
		case meLabel != "" && me.isMe(p.segs):
			label := meLabel
			entry.Matched = &label
			entry.Cluster = label
			entry.ChannelMatched = true
		case entry.Matched != nil:
			entry.Cluster = *entry.Matched
		default:
			if unnamed < 26 {
				entry.Cluster = fmt.Sprintf("Speaker %c", 'A'+unnamed)
			} else {
				entry.Cluster = fmt.Sprintf("Speaker %d", unnamed+1)
			}
			unnamed++
		}
		for _, l := range p.labels {
			labelMap[l] = entry.Cluster
		}
		clusters = append(clusters, entry)
	}
	return clusters, labelMap
}

// resolveMeName maps a hinted name to its enrolled display form when one matches
// (via NormalizeName), so the label agrees with the speaker's profile; otherwise
// the name is used verbatim — the channel is proof they spoke, enrolled or not.
func (d *Diarizer) resolveMeName(name string) string {
	names, _ := d.store.ListSpeakers()
	norm := NormalizeName(name)
	for _, e := range names {
		if NormalizeName(e) == norm {
			return e
		}
	}
	return strings.TrimSpace(name)
}

// attendeesApplied renders the resolved attendee set for the report (sorted, or nil).
func attendeesApplied(set map[string]bool) []string {
	if set == nil {
		return nil
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// IdentifySpeakers runs diarization + identification only — no transcription.
// samples are 16 kHz mono.
func (d *Diarizer) IdentifySpeakers(samples []float32, threshold float64, attendees []string, me *MeHint) *SpeakerReport {
	set := d.resolveAttendees(attendees)
	timeline := d.Timeline(samples)
	clusters, _ := d.analyzeAndLabel(samples, timeline, threshold, set, me)
	return buildReport(clusters, threshold, attendeesApplied(set))
}

// DiarizeWords attributes transcribed words to speakers and builds the report.
// Ports the word-midpoint assignment, UNKNOWN fill, and segment grouping from
// Diarizer.diarize in diarizer.py.
func (d *Diarizer) DiarizeWords(samples []float32, words []Word, threshold float64, attendees []string, me *MeHint) ([]Segment, *SpeakerReport) {
	set := d.resolveAttendees(attendees)
	timeline := d.Timeline(samples)
	clusters, labelMap := d.analyzeAndLabel(samples, timeline, threshold, set, me)
	// Turns from dropped micro-clusters are removed so their words go through the
	// UNKNOWN-fill path rather than surfacing a label the report no longer has.
	attributable := make([]Turn, 0, len(timeline))
	for _, t := range timeline {
		if _, ok := labelMap[t.Label]; ok {
			attributable = append(attributable, t)
		}
	}
	segments := attributeWords(words, attributable, labelMap)
	return segments, buildReport(clusters, threshold, attendeesApplied(set))
}

// EnrollSpeaker computes and stores a voice profile from a reference clip — the
// whole clip embedded as one span, same as the Python service's /enroll.
func (d *Diarizer) EnrollSpeaker(name string, samples []float32) error {
	emb := d.embed(samples, 0, float64(len(samples))/pipelineRate)
	if emb == nil {
		return fmt.Errorf("clip too short to compute a voice profile")
	}
	return d.store.Save(name, emb)
}

// Transcribe is the full /transcribe pipeline: diarize, transcribe the speech
// (windowed at diarization gaps), attribute words, and build the report.
// Transcription windows cover the FULL timeline — including micro-fragments the
// report drops — so no speech goes untranscribed; their words then flow through
// the UNKNOWN-fill path like any other unattributed word.
func (d *Diarizer) Transcribe(t Transcriber, samples []float32, threshold float64, attendees []string, me *MeHint) ([]Segment, *SpeakerReport) {
	set := d.resolveAttendees(attendees)
	timeline := d.Timeline(samples)
	clusters, labelMap := d.analyzeAndLabel(samples, timeline, threshold, set, me)
	words := TranscribeMeeting(t, samples, timeline)
	attributable := make([]Turn, 0, len(timeline))
	for _, t := range timeline {
		if _, ok := labelMap[t.Label]; ok {
			attributable = append(attributable, t)
		}
	}
	segments := attributeWords(words, attributable, labelMap)
	return segments, buildReport(clusters, threshold, attendeesApplied(set))
}
