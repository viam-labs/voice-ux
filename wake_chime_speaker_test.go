package voiceux

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/components/audioout"
	"go.viam.com/rdk/logging"
	rutils "go.viam.com/rdk/utils"
	"go.viam.com/test"
)

// fakeSpeaker records Play calls. Only Play is called.
type fakeSpeaker struct {
	audioout.AudioOut
	mu     sync.Mutex
	played [][]byte
}

func (f *fakeSpeaker) Play(_ context.Context, data []byte, _ *rutils.AudioInfo, _ map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.played = append(f.played, data)
	return nil
}

func (f *fakeSpeaker) names(c *wakeChimeSpeaker) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.played))
	for _, pcm := range f.played {
		name := "?"
		for n, s := range c.sounds {
			if bytes.Equal(s, pcm) {
				name = n
				break
			}
		}
		out = append(out, name)
	}
	return out
}

func newTestChimes(t *testing.T, speaker *fakeSpeaker) *wakeChimeSpeaker {
	t.Helper()
	sounds, err := loadSounds(context.Background(), &Config{})
	test.That(t, err, test.ShouldBeNil)
	return &wakeChimeSpeaker{
		logger:    logging.NewTestLogger(t),
		speaker:   speaker,
		sounds:    sounds,
		playStart: true,
		playEnd:   true,
		enabled:   true,
	}
}

func audioChunk(data ...byte) *audioin.AudioChunk {
	return &audioin.AudioChunk{AudioData: data}
}

// feed runs drain over the given chunks and returns after it exits.
func feed(t *testing.T, c *wakeChimeSpeaker, chunks ...*audioin.AudioChunk) {
	t.Helper()
	ch := make(chan *audioin.AudioChunk, len(chunks))
	for _, chunk := range chunks {
		ch <- chunk
	}
	close(ch)
	c.drain(context.Background(), ch)
}

func TestDrainPlaysStartAndEndCues(t *testing.T) {
	speaker := &fakeSpeaker{}
	c := newTestChimes(t, speaker)

	// One segment: two audio chunks then the empty-chunk sentinel.
	feed(t, c, audioChunk(1, 2), audioChunk(3, 4), audioChunk())

	test.That(t, speaker.names(c), test.ShouldResemble,
		[]string{StartListeningSound, EndListeningSound})
}

func TestDrainSuppressesHotMicFollowUps(t *testing.T) {
	speaker := &fakeSpeaker{}
	c := newTestChimes(t, speaker)
	c.followupWindow = time.Hour

	// Wake segment, then two follow-up segments inside the window: the
	// follow-ups stay fully silent, and each still refreshes the window.
	feed(t, c,
		audioChunk(1), audioChunk(),
		audioChunk(2), audioChunk(),
		audioChunk(3), audioChunk(),
	)

	test.That(t, speaker.names(c), test.ShouldResemble,
		[]string{StartListeningSound, EndListeningSound})
}

func TestDrainCuesEverySegmentByDefault(t *testing.T) {
	speaker := &fakeSpeaker{}
	c := newTestChimes(t, speaker)

	// followupWindow defaults to 0: hot-mic follow-ups are cued too.
	feed(t, c,
		audioChunk(1), audioChunk(),
		audioChunk(2), audioChunk(),
	)

	test.That(t, speaker.names(c), test.ShouldResemble, []string{
		StartListeningSound, EndListeningSound,
		StartListeningSound, EndListeningSound,
	})
}

func TestDrainDisabledPlaysNothing(t *testing.T) {
	speaker := &fakeSpeaker{}
	c := newTestChimes(t, speaker)
	c.enabled = false

	feed(t, c, audioChunk(1), audioChunk())

	test.That(t, len(speaker.played), test.ShouldEqual, 0)
}

func TestDrainEndSoundOnly(t *testing.T) {
	speaker := &fakeSpeaker{}
	c := newTestChimes(t, speaker)
	c.playStart = false

	feed(t, c, audioChunk(1), audioChunk())

	test.That(t, speaker.names(c), test.ShouldResemble, []string{EndListeningSound})
}

func TestDrainIgnoresSentinelOutsideSegment(t *testing.T) {
	speaker := &fakeSpeaker{}
	c := newTestChimes(t, speaker)

	// Sentinels with no preceding segment (e.g. joining mid-stream) are
	// ignored.
	feed(t, c, audioChunk(), audioChunk())

	test.That(t, len(speaker.played), test.ShouldEqual, 0)
}

func TestSetEnabledViaDoCommand(t *testing.T) {
	speaker := &fakeSpeaker{}
	c := newTestChimes(t, speaker)

	resp, err := c.DoCommand(context.Background(), map[string]interface{}{
		"command": "set_enabled", "enabled": false,
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["enabled"], test.ShouldBeFalse)

	feed(t, c, audioChunk(1), audioChunk())
	test.That(t, len(speaker.played), test.ShouldEqual, 0)
}

func TestDoCommandPlay(t *testing.T) {
	speaker := &fakeSpeaker{}
	c := newTestChimes(t, speaker)

	resp, err := c.DoCommand(context.Background(), map[string]interface{}{
		"command": "play", "sound": EndListeningSound,
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["played"], test.ShouldEqual, EndListeningSound)
	test.That(t, speaker.names(c), test.ShouldResemble, []string{EndListeningSound})

	_, err = c.DoCommand(context.Background(), map[string]interface{}{
		"command": "play", "sound": "nope",
	})
	test.That(t, err, test.ShouldNotBeNil)
}

func TestPlayPassesThroughToWrappedSpeaker(t *testing.T) {
	speaker := &fakeSpeaker{}
	c := newTestChimes(t, speaker)

	appAudio := []byte{9, 9, 9}
	err := c.Play(context.Background(), appAudio, &rutils.AudioInfo{Codec: rutils.CodecPCM16}, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(speaker.played), test.ShouldEqual, 1)
	test.That(t, speaker.played[0], test.ShouldResemble, appAudio)
}

func TestResolveSoundURLDownloadsAndCaches(t *testing.T) {
	want := []byte{1, 2, 3, 4}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Write(want)
	}))
	defer srv.Close()
	t.Setenv("VIAM_MODULE_DATA", t.TempDir())

	got, err := resolveSound(context.Background(), srv.URL+"/chime.pcm")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got, test.ShouldResemble, want)

	// Second resolve must come from the cache, not the network.
	got, err = resolveSound(context.Background(), srv.URL+"/chime.pcm")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got, test.ShouldResemble, want)
	test.That(t, hits.Load(), test.ShouldEqual, int32(1))
}

func TestResolveSoundURLHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("VIAM_MODULE_DATA", t.TempDir())

	_, err := resolveSound(context.Background(), srv.URL+"/missing.pcm")
	test.That(t, err, test.ShouldNotBeNil)
}

func TestValidate(t *testing.T) {
	_, _, err := (&Config{Speaker: "spk"}).Validate("attrs")
	test.That(t, err, test.ShouldNotBeNil)

	_, _, err = (&Config{Mic: "mic"}).Validate("attrs")
	test.That(t, err, test.ShouldNotBeNil)

	deps, _, err := (&Config{Mic: "mic", Speaker: "spk"}).Validate("attrs")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"mic", "spk"})
}
