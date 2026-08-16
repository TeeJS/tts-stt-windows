# Meetings API — sending recordings for diarized transcription

tts-sst can transcribe meeting recordings with named speakers. A sending
application needs only **an IP address and a port** — by default port **10301**.
The contract is wire-compatible with the Python meeting-diarizer, so clients
written for either work against both.

## Reaching the service

- **Same machine:** `http://127.0.0.1:10301`
- **Another machine:** the tts-sst host must set `"bind": "0.0.0.0"` in
  `%APPDATA%\tts-sst\config.json` (default is loopback-only; the Meetings
  settings tab shows a warning while that's the case). Then use
  `http://<host-ip>:10301`.
- The port is `meetingsPort` in the same config file.
- **Liveness check:** `GET /health` → `200 {"status":"ok"}`. If the connection
  is refused, the meetings service is switched off in the tts-sst settings page.

## Sending a recording

```
POST /transcribe
Content-Type: multipart/form-data
```

| field       | type   | required | notes |
|-------------|--------|----------|-------|
| `audio`     | file   | yes      | WAV (PCM 16/24/32-bit or float32, any rate/channels) or MP3 — including MP3 data inside a `.wav` container (HiDock recorders do this). Anything else → 400. |
| `threshold` | text   | no       | Speaker-identification cosine cutoff, e.g. `0.70`. Defaults to the service's configured value. |
| `attendees` | text   | no       | Comma-separated names. Enrolled speakers *not* on the list are penalized 0.15, reducing false matches. `"Last, First"` and `"First Last"` forms both work. |
| `me_name`   | text   | no       | Name of the speaker isolated on one channel of a **stereo** recording (e.g. your mic on the left, everyone else's loopback on the right). That speaker's cluster is labeled from channel energy — ground truth, no cosine threshold, correct even on a bad-mic day. Ignored on a mono file. Need not be enrolled. |
| `me_channel`| text   | no       | Which channel holds `me_name`'s mic: `left` (default) or `right`. |

**Channel-guided identification:** when `me_name` is set and the upload is stereo,
the service measures each diarized cluster's speech energy on the mic channel vs the
other channel; the cluster that's mic-dominant (≥3×) is labeled `me_name` outright,
overriding voice matching (the cosine scores stay in the report). Its cluster carries
`"channel_matched": true`. Bulletproof for remote meetings where the loopback excludes
your voice; in a hybrid room where people sit with you, their voice reaches your mic
too, so those fall back to voice matching. Absent `me_name` or a mono file → normal
voice matching, unchanged.

**The request blocks until processing finishes.** Budget roughly a third of the
recording's duration on CPU (an 18-minute meeting takes ~5 minutes). Set your
HTTP timeout to 3600 s. One job runs at a time; concurrent requests queue.

### Response `200 OK`

```json
{
  "speaker_report": {
    "threshold_used": 0.7,
    "attendees_applied": null,
    "speaker_count": 4,
    "total_speech_sec": 658.0,
    "speech_named_pct": 37.7,
    "speakers": [
      {"label": "T.J. Schmitz", "identified": true, "duration_sec": 248.38,
       "clusters": 1, "best_score": 0.9161, "nearest": "T.J. Schmitz", "speech_pct": 37.7}
    ],
    "enrollment_candidates": [ /* unidentified speakers worth enrolling */ ],
    "clusters": [ /* per-cluster detail: scores vs every enrolled speaker, margins, spans */ ],
    "threshold_sweep": [ /* what other thresholds would have produced */ ]
  },
  "segments": [
    {"speaker": "T.J. Schmitz", "start": 179.9, "end": 205.02, "text": "..."},
    {"speaker": "Speaker A",    "start": 221.05, "end": 225.53, "text": "..."},
    {"speaker": "UNKNOWN",      "start": 231.45, "end": 233.37, "text": "..."}
  ]
}
```

`segments` is ordered by time. `speaker` is an enrolled name, `Speaker A`/`B`/…
for consistent-but-unknown voices, or the literal `UNKNOWN` for spans at genuine
speaker handovers.

The response also carries a suggested filename following the pipeline's naming
convention — `Content-Disposition: inline; filename="<recording basename>-diarizer-response.json"`
(e.g. `My Meeting 2026.wav` → `My Meeting 2026-diarizer-response.json`). Honor
it when saving, or name the file yourself; either way the body is the same.

### Errors

Errors mirror FastAPI: a JSON body `{"detail": "<message>"}` with status 400
(bad upload, unsupported audio, bad field) or 500 (processing failure).

## Minimal client examples

curl:

```bash
curl -X POST -F "audio=@meeting.wav" -F "threshold=0.70" \
  --max-time 3600 http://HOST:10301/transcribe -o result.json
```

Python (stdlib only — or use the existing `client/diarize-transcribe.py`,
which works unchanged):

```python
import urllib.request, json

boundary = "----MeetingHandoff"
with open("meeting.wav", "rb") as f:
    audio = f.read()
body = (
    f"--{boundary}\r\nContent-Disposition: form-data; name=\"audio\"; "
    f"filename=\"meeting.wav\"\r\nContent-Type: application/octet-stream\r\n\r\n"
).encode() + audio + f"\r\n--{boundary}--\r\n".encode()
req = urllib.request.Request(
    "http://HOST:10301/transcribe", data=body,
    headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
result = json.load(urllib.request.urlopen(req, timeout=3600))
for seg in result["segments"]:
    print(f'{seg["speaker"]}: {seg["text"]}')
```

## Other endpoints

| endpoint | method | purpose |
|---|---|---|
| `/identify` | POST | Same fields as `/transcribe`, but no transcription — returns the bare `speaker_report` object (unwrapped). Fast way to check who's in a clip. |
| `/enroll` | POST | multipart `name` + `audio`: creates/replaces a voice profile from the clip (45+ s of one person, recorded on the hardware meetings actually use). → `{"status":"enrolled","name":...}` |
| `/speakers` | GET | `{"speakers":[names...], "details":[{"name","enrolled_at"},...]}` |
| `/speakers/{name}/rename` | POST | form field `new_name` → 200 / 404 / 409 |
| `/speakers/{name}` | DELETE | remove a profile |

Speakers can also be enrolled and managed interactively from the tts-sst
settings page (tray → Settings → Meetings), so end users never need these
endpoints directly.
