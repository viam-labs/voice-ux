// Package voiceux provides voice-assistant UX components.
//
// wake-chimes is a drop-in AudioOut wrapper: it passes normal playback
// through to the wrapped speaker, and plays the two standard attention cues —
// start-of-listening and end-of-listening — by subscribing to a wake-word
// filter microphone (any AudioIn that emits speech segments delimited by an
// empty-chunk sentinel, e.g. viam:filtered-audio:wake-word-filter). In
// Wyoming-protocol terms it reacts to `detection` (first chunk of a segment)
// and `audio-stop` (the sentinel). The subscribed audio is discarded; only
// the boundaries matter. It mirrors the wake-word filter's own shape: that
// model wraps the mic, this one wraps the speaker.
package voiceux

import (
	"context"
	"embed"
	"fmt"
	"os"
	"sync"
	"time"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/components/audioout"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	rutils "go.viam.com/rdk/utils"
	goutils "go.viam.com/utils"
)

// WakeChimeSpeaker is the model triplet for the chime player.
var WakeChimeSpeaker = resource.NewModel("viam", "voice-ux", "wake-chime-speaker")

//go:embed sounds/*.pcm
var defaultSounds embed.FS

// Sound names, used as DoCommand "play" arguments and embedded filenames.
const (
	StartListeningSound = "start_listening"
	EndListeningSound   = "end_listening"
)

// All sounds are raw PCM16: 16 kHz mono little-endian. Config overrides must
// be in the same format.
const (
	soundSampleRateHz = 16000
	soundNumChannels  = 1
)

const (
	defaultMinIntervalSeconds = 2.0
	playTimeout               = 5 * time.Second
	reconnectDelay            = time.Second
)

func init() {
	resource.RegisterComponent(audioout.API, WakeChimeSpeaker,
		resource.Registration[audioout.AudioOut, *Config]{
			Constructor: newWakeChimeSpeaker,
		},
	)
}

// Config holds the model's machine.json attributes.
type Config struct {
	// Mic is the Viam resource name of the wake-word-filter AudioIn to
	// subscribe to. Required.
	Mic string `json:"mic"`

	// Speaker is the Viam resource name of the AudioOut that plays the
	// cues. Required.
	Speaker string `json:"speaker"`

	// PlayStartSound / PlayEndSound toggle the two cues. Both default to
	// true (the convention for devices without a strong visual indicator).
	PlayStartSound *bool `json:"play_start_sound,omitempty"`
	PlayEndSound   *bool `json:"play_end_sound,omitempty"`

	// StartSound / EndSound are optional paths to raw PCM16 files
	// (16 kHz mono little-endian) replacing the embedded default earcons.
	StartSound string `json:"start_sound,omitempty"`
	EndSound   string `json:"end_sound,omitempty"`

	// MinIntervalSeconds debounces cueing: segments that start within this
	// window of the previous cued segment are not cued. Guards against
	// re-chiming on every follow-up turn when the source filter runs in
	// conversation mode. Defaults to 2.0.
	MinIntervalSeconds float64 `json:"min_interval_seconds,omitempty"`
}

// Validate declares dependencies and validates required fields.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Mic == "" {
		return nil, nil, fmt.Errorf("%s: mic is required", path)
	}
	if cfg.Speaker == "" {
		return nil, nil, fmt.Errorf("%s: speaker is required", path)
	}
	if cfg.MinIntervalSeconds < 0 {
		return nil, nil, fmt.Errorf("%s: min_interval_seconds must be non-negative", path)
	}
	return []string{cfg.Mic, cfg.Speaker}, nil, nil
}

type wakeChimeSpeaker struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger
	cfg    *Config

	mic     audioin.AudioIn
	speaker audioout.AudioOut
	sounds  map[string][]byte // raw PCM16, 16 kHz mono

	playStart   bool
	playEnd     bool
	minInterval time.Duration

	mu         sync.Mutex
	enabled    bool
	subscribed bool
	lastWake   time.Time // last segment start, cued or not (for status)
	lastCuedAt time.Time // last segment start that was cued (for debounce)

	workers *goutils.StoppableWorkers
}

func newWakeChimeSpeaker(
	_ context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger,
) (audioout.AudioOut, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	mic, err := audioin.FromProvider(deps, conf.Mic)
	if err != nil {
		return nil, fmt.Errorf("mic %q not found: %w", conf.Mic, err)
	}
	speaker, err := audioout.FromProvider(deps, conf.Speaker)
	if err != nil {
		return nil, fmt.Errorf("speaker %q not found: %w", conf.Speaker, err)
	}

	sounds, err := loadSounds(conf)
	if err != nil {
		return nil, err
	}

	minInterval := conf.MinIntervalSeconds
	if minInterval == 0 {
		minInterval = defaultMinIntervalSeconds
	}

	c := &wakeChimeSpeaker{
		name:        rawConf.ResourceName(),
		logger:      logger,
		cfg:         conf,
		mic:         mic,
		speaker:     speaker,
		sounds:      sounds,
		playStart:   boolOr(conf.PlayStartSound, true),
		playEnd:     boolOr(conf.PlayEndSound, true),
		minInterval: time.Duration(minInterval * float64(time.Second)),
		enabled:     true,
	}
	c.workers = goutils.NewBackgroundStoppableWorkers(c.runListener)

	logger.Infof("wake-chimes ready: mic=%s speaker=%s start=%t end=%t min_interval=%s",
		conf.Mic, conf.Speaker, c.playStart, c.playEnd, c.minInterval)
	return c, nil
}

func (c *wakeChimeSpeaker) Name() resource.Name {
	return c.name
}

// Play passes app audio (e.g. TTS) through to the wrapped speaker, making
// this model a drop-in speaker replacement.
func (c *wakeChimeSpeaker) Play(ctx context.Context, data []byte, info *rutils.AudioInfo, extra map[string]interface{}) error {
	return c.speaker.Play(ctx, data, info, extra)
}

// PlayStream passes streamed app audio through to the wrapped speaker.
func (c *wakeChimeSpeaker) PlayStream(ctx context.Context, info *rutils.AudioInfo, chunks <-chan []byte, extra map[string]interface{}) error {
	return c.speaker.PlayStream(ctx, info, chunks, extra)
}

// Properties reports the wrapped speaker's properties.
func (c *wakeChimeSpeaker) Properties(ctx context.Context, extra map[string]interface{}) (rutils.Properties, error) {
	return c.speaker.Properties(ctx, extra)
}

// runListener holds a subscription to the filter mic for the lifetime of the
// component, reconnecting on stream end or error.
func (c *wakeChimeSpeaker) runListener(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		chunks, err := c.mic.GetAudio(ctx, rutils.CodecPCM16, 0, 0, nil)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logger.Warnf("mic.GetAudio: %v; retrying in %s", err, reconnectDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
			}
			continue
		}

		c.setSubscribed(true)
		c.drain(ctx, chunks)
		c.setSubscribed(false)
	}
}

// drain consumes one subscription, cueing on segment boundaries: the first
// non-empty chunk after a boundary means the wake word fired upstream
// (Wyoming: detection); an empty chunk is the segment-end sentinel (Wyoming:
// audio-stop). A segment's start and end cues are gated together so a
// debounced follow-up segment stays fully silent.
func (c *wakeChimeSpeaker) drain(ctx context.Context, chunks <-chan *audioin.AudioChunk) {
	inSegment := false
	cued := false
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-chunks:
			if !ok {
				return
			}
			if chunk == nil {
				continue
			}
			if len(chunk.AudioData) == 0 {
				if inSegment {
					inSegment = false
					if cued && c.playEnd {
						c.play(ctx, EndListeningSound)
					}
					cued = false
				}
				continue
			}
			if !inSegment {
				inSegment = true
				cued = c.segmentStarted()
				if cued && c.playStart {
					c.play(ctx, StartListeningSound)
				}
			}
		}
	}
}

// segmentStarted records the wake and reports whether this segment should be
// cued (enabled and outside the debounce window).
func (c *wakeChimeSpeaker) segmentStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.lastWake = now
	if !c.enabled {
		return false
	}
	if !c.lastCuedAt.IsZero() && now.Sub(c.lastCuedAt) < c.minInterval {
		return false
	}
	c.lastCuedAt = now
	return true
}

func (c *wakeChimeSpeaker) setSubscribed(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscribed = v
}

func (c *wakeChimeSpeaker) play(ctx context.Context, name string) {
	pcm, ok := c.sounds[name]
	if !ok {
		c.logger.Warnf("unknown sound %q", name)
		return
	}
	playCtx, cancel := context.WithTimeout(ctx, playTimeout)
	defer cancel()
	info := &rutils.AudioInfo{
		Codec:        rutils.CodecPCM16,
		SampleRateHz: soundSampleRateHz,
		NumChannels:  soundNumChannels,
	}
	if err := c.speaker.Play(playCtx, pcm, info, nil); err != nil {
		c.logger.Warnf("play %s: %v", name, err)
	}
}

// DoCommand supports:
//
//	{"command": "play", "sound": "start_listening"|"end_listening"}
//	{"command": "set_enabled", "enabled": bool}
//	{"command": "status"}
func (c *wakeChimeSpeaker) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "play":
		name, _ := cmd["sound"].(string)
		if _, ok := c.sounds[name]; !ok {
			return nil, fmt.Errorf("unknown sound %q (available: %s, %s)",
				name, StartListeningSound, EndListeningSound)
		}
		c.play(ctx, name)
		return map[string]interface{}{"played": name}, nil

	case "set_enabled":
		enabled, ok := cmd["enabled"].(bool)
		if !ok {
			return nil, fmt.Errorf("set_enabled requires a boolean 'enabled' field")
		}
		c.mu.Lock()
		c.enabled = enabled
		c.mu.Unlock()
		return map[string]interface{}{"enabled": enabled}, nil

	case "status":
		return c.status(), nil

	default:
		return nil, fmt.Errorf("unknown command %q (supported: play, set_enabled, status)", command)
	}
}

// Status reports the same fields as the "status" DoCommand.
func (c *wakeChimeSpeaker) Status(_ context.Context) (map[string]interface{}, error) {
	return c.status(), nil
}

func (c *wakeChimeSpeaker) status() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	lastWakeMsAgo := int64(-1)
	if !c.lastWake.IsZero() {
		lastWakeMsAgo = time.Since(c.lastWake).Milliseconds()
	}
	return map[string]interface{}{
		"enabled":          c.enabled,
		"subscribed":       c.subscribed,
		"play_start_sound": c.playStart,
		"play_end_sound":   c.playEnd,
		"last_wake_ms_ago": lastWakeMsAgo,
	}
}

func (c *wakeChimeSpeaker) Close(_ context.Context) error {
	c.workers.Stop()
	return nil
}

func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// loadSounds returns the start/end sounds as raw PCM16: embedded defaults,
// with config path overrides applied.
func loadSounds(cfg *Config) (map[string][]byte, error) {
	sounds := make(map[string][]byte, 2)
	for name, override := range map[string]string{
		StartListeningSound: cfg.StartSound,
		EndListeningSound:   cfg.EndSound,
	} {
		var pcm []byte
		var err error
		if override != "" {
			pcm, err = os.ReadFile(override)
			if err != nil {
				return nil, fmt.Errorf("read %s override: %w", name, err)
			}
		} else {
			pcm, err = defaultSounds.ReadFile("sounds/" + name + ".pcm")
			if err != nil {
				return nil, fmt.Errorf("read embedded %s: %w", name, err)
			}
		}
		if len(pcm) == 0 || len(pcm)%2 != 0 {
			return nil, fmt.Errorf("%s sound is not valid PCM16 (got %d bytes)", name, len(pcm))
		}
		sounds[name] = pcm
	}
	return sounds, nil
}
