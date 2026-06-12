# voice-ux

Voice assistant UX components for Viam machines.

Voice assistants use non-verbal audio cues (earcons) to communicate attention
state: a sound when the assistant starts listening and another when it stops.
This module provides that layer for Viam voice stacks built on
[`viam:filtered-audio:wake-word-filter`](https://github.com/viam-modules/filtered-audio)
and [`viam:speech-to-text`](https://github.com/viam-labs/speech-to-text).

## Model viam:voice-ux:wake-chime-speaker

A drop-in `audioout` (speaker) wrapper. Normal playback (`Play`/`PlayStream`)
passes through to the wrapped hardware speaker. In addition, the model
subscribes to a wake-word-filter microphone and plays the two standard
attention cues:

- **start-of-listening** — when the wake word fires (the first audio chunk of
  a speech segment arrives)
- **end-of-listening** — when the speech segment ends (the empty-chunk
  sentinel arrives)

The subscribed audio is discarded; only the segment boundaries matter. The
source mic must be 16 kHz mono PCM16 with empty-chunk segment delimiters
(what the wake-word filter emits). Requires a wake-word filter that supports
multiple concurrent `GetAudio` clients.

### Configuration

```json
{
  "name": "chime-speaker",
  "api": "rdk:component:audioout",
  "model": "viam:voice-ux:wake-chime-speaker",
  "attributes": {
    "mic": "filtered-mic",
    "speaker": "viam-speaker"
  },
  "depends_on": ["filtered-mic", "viam-speaker"]
}
```

| Attribute | Type | Required | Default | Description |
|---|---|---|---|---|
| `mic` | string | yes | — | Wake-word-filter `audioin` to subscribe to. |
| `speaker` | string | yes | — | Hardware `audioout` that plays everything. |
| `play_start_sound` | bool | no | `true` | Play the start-of-listening cue on wake. |
| `play_end_sound` | bool | no | `true` | Play the end-of-listening cue at segment end. |
| `start_sound` | string | no | embedded | Path to a raw PCM16 file (16 kHz mono LE) replacing the default start cue. |
| `end_sound` | string | no | embedded | Path to a raw PCM16 file (16 kHz mono LE) replacing the default end cue. |
| `min_interval_seconds` | float | no | `2.0` | Segments starting within this window of the last cued segment are not cued (avoids re-chiming on conversation-mode follow-ups). |

### DoCommand

```json
{"command": "play", "sound": "start_listening"}
{"command": "set_enabled", "enabled": false}
{"command": "status"}
```

`set_enabled` lets an application that owns sound-routing policy (e.g. one
that sends cues to a phone app instead) turn the automatic chimes off at
runtime without a reconfigure. `status` reports `enabled`, `subscribed`,
the cue toggles, and `last_wake_ms_ago`.

### Default sounds

The embedded earcons are a mirrored two-tone pair — ascending (A5→E6) for
start, descending for end — following the convention used by the major voice
assistants. Regenerate with:

```bash
make sounds
```

## Build and test

```bash
make setup   # go mod tidy
make test
make module.tar.gz
```
