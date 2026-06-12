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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
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
	playTimeout    = 5 * time.Second
	reconnectDelay = time.Second
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

	// StartSound / EndSound replace the embedded default earcons. Each is
	// either a local path or an http(s) URL (e.g. a raw GitHub link) to a
	// sound file (16 kHz mono: .wav or raw PCM16). The bytes are passed to
	// the speaker as-is. URLs are downloaded once and cached under
	// VIAM_MODULE_DATA.
	StartSound string `json:"start_sound,omitempty"`
	EndSound   string `json:"end_sound,omitempty"`

	// FollowupWindowSeconds silences cues on hot-mic follow-up turns:
	// segments starting within this window of the previous segment's end
	// are not cued, and every segment end refreshes the window. Set it
	// equal to the source filter's conversation_timeout_seconds. 0
	// (default) cues every segment.
	FollowupWindowSeconds float64 `json:"followup_window_seconds,omitempty"`
}

// Validate declares dependencies and validates required fields.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Mic == "" {
		return nil, nil, fmt.Errorf("%s: mic is required", path)
	}
	if cfg.Speaker == "" {
		return nil, nil, fmt.Errorf("%s: speaker is required", path)
	}
	if cfg.FollowupWindowSeconds < 0 {
		return nil, nil, fmt.Errorf("%s: followup_window_seconds must be non-negative", path)
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

	playStart      bool
	playEnd        bool
	followupWindow time.Duration // mirrors the filter's conversation window; segments inside it are follow-ups

	mu             sync.Mutex
	enabled        bool
	subscribed     bool
	lastWake       time.Time // last segment start, cued or not (for status)
	lastSegmentEnd time.Time // refreshed on every segment end (follow-up window anchor)

	workers *goutils.StoppableWorkers
}

func newWakeChimeSpeaker(
	ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger,
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

	sounds, err := loadSounds(ctx, conf)
	if err != nil {
		return nil, err
	}

	c := &wakeChimeSpeaker{
		name:           rawConf.ResourceName(),
		logger:         logger,
		cfg:            conf,
		mic:            mic,
		speaker:        speaker,
		sounds:         sounds,
		playStart:      boolOr(conf.PlayStartSound, true),
		playEnd:        boolOr(conf.PlayEndSound, true),
		followupWindow: time.Duration(conf.FollowupWindowSeconds * float64(time.Second)),
		enabled:        true,
	}
	c.workers = goutils.NewBackgroundStoppableWorkers(c.runListener)

	logger.Infof("wake-chimes ready: mic=%s speaker=%s start=%t end=%t followup_window=%s",
		conf.Mic, conf.Speaker, c.playStart, c.playEnd, c.followupWindow)
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
					c.segmentEnded()
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
// cued: enabled, and not a hot-mic follow-up (a segment starting inside the
// follow-up window of the previous segment's end).
func (c *wakeChimeSpeaker) segmentStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.lastWake = now
	if !c.enabled {
		return false
	}
	if c.followupWindow > 0 && !c.lastSegmentEnd.IsZero() &&
		now.Sub(c.lastSegmentEnd) < c.followupWindow {
		return false
	}
	return true
}

// segmentEnded refreshes the follow-up window. Every segment refreshes it —
// cued or not — matching how the source filter extends its conversation
// window on each yielded segment.
func (c *wakeChimeSpeaker) segmentEnded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSegmentEnd = time.Now()
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

	default:
		return nil, fmt.Errorf("unknown command %q (supported: play, set_enabled)", command)
	}
}

// Status reports enabled, subscribed, the cue toggles, and last_wake_ms_ago.
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
// with config path/URL overrides applied.
func loadSounds(ctx context.Context, cfg *Config) (map[string][]byte, error) {
	sounds := make(map[string][]byte, 2)
	for name, override := range map[string]string{
		StartListeningSound: cfg.StartSound,
		EndListeningSound:   cfg.EndSound,
	} {
		var pcm []byte
		var err error
		if override != "" {
			pcm, err = resolveSound(ctx, override)
			if err != nil {
				return nil, fmt.Errorf("load %s override: %w", name, err)
			}
		} else {
			pcm, err = defaultSounds.ReadFile("sounds/" + name + ".pcm")
			if err != nil {
				return nil, fmt.Errorf("read embedded %s: %w", name, err)
			}
		}
		if len(pcm) == 0 {
			return nil, fmt.Errorf("%s sound is empty", name)
		}
		sounds[name] = pcm
	}
	return sounds, nil
}

// resolveSound reads a sound override from a local path or, for URLs (e.g. a
// raw GitHub link), from a download saved under VIAM_MODULE_DATA. The saved
// file is named by the URL's basename, normalized to a .pcm extension, and
// reused if it already exists.
func resolveSound(ctx context.Context, pathOrURL string) ([]byte, error) {
	if !isValidURL(pathOrURL) {
		return os.ReadFile(pathOrURL)
	}

	filePath := filepath.Join(os.Getenv("VIAM_MODULE_DATA"), path.Base(pathOrURL))

	// check if the sound was already downloaded
	if _, err := os.Stat(filePath); err == nil {
		return os.ReadFile(filePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	httpClient := &http.Client{Timeout: 25 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pathOrURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", pathOrURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", pathOrURL, res.StatusCode)
	}
	pcm, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", pathOrURL, err)
	}
	if err := os.WriteFile(filePath, pcm, 0o644); err != nil {
		return nil, err
	}
	return pcm, nil
}

func isValidURL(str string) bool {
	parsedURL, err := url.ParseRequestURI(str)
	return err == nil && parsedURL.Scheme != "" && parsedURL.Host != ""
}
