// Command gen_sounds regenerates the embedded default earcons as raw PCM16
// (16 kHz mono, little-endian).
//
// The cues follow the convention shared by the major voice assistants: a
// mirrored two-note pair, rising for "started listening" and falling for
// "done listening". Notes are synthesized as soft bell tones
//
// Run from the repo root:
//
//	go run ./etc/gen_sounds
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

const (
	sampleRate = 16000
	amplitude  = 0.45
	attackSec  = 0.006
)

// partial is one harmonic component of a bell note: a frequency multiple,
// its relative level, and how fast it dies away relative to the fundamental.
type partial struct {
	ratio, level, decayMul float64
}

// Soft bell/marimba voicing: strong fundamental, quieter upper partials that
// decay faster (higher partials ringing too long is what makes a tone harsh).
var bellPartials = []partial{
	{ratio: 1.0, level: 1.0, decayMul: 1.0},
	{ratio: 2.0, level: 0.35, decayMul: 1.6},
	{ratio: 3.0, level: 0.12, decayMul: 2.4},
}

// note renders a bell tone at freqHz into out starting at startSec, ringing
// for ringSec. Overlapping notes sum, which is what makes the pair legato.
func note(out []float64, startSec, freqHz, ringSec float64) {
	start := int(startSec * sampleRate)
	n := int(ringSec * sampleRate)
	attackN := int(attackSec * sampleRate)
	for i := 0; i < n && start+i < len(out); i++ {
		t := float64(i) / sampleRate
		env := 1.0
		if i < attackN {
			env = float64(i) / float64(attackN)
		}
		var s float64
		for _, p := range bellPartials {
			f := freqHz * p.ratio
			if f >= sampleRate/2 {
				continue
			}
			decay := math.Exp(-6 * t * p.decayMul / ringSec)
			s += p.level * decay * math.Sin(2*math.Pi*f*t)
		}
		out[start+i] += amplitude * env * s / 1.47 // normalize partial levels
	}
}

func render(notes [][3]float64, totalSec float64) []float64 {
	out := make([]float64, int(totalSec*sampleRate))
	for _, n := range notes {
		note(out, n[0], n[1], n[2])
	}
	return out
}

func pcm16(samples []float64) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		v := int16(math.Max(-1, math.Min(1, s)) * 32767)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

func main() {
	// Rising fourth (G5 → C6) opens; the mirrored falling pair closes.
	// Each [start sec, freq Hz, ring sec].
	start := render([][3]float64{
		{0.00, 783.99, 0.28},
		{0.11, 1046.50, 0.34},
	}, 0.50)

	end := render([][3]float64{
		{0.00, 1046.50, 0.28},
		{0.11, 783.99, 0.34},
	}, 0.50)

	for path, samples := range map[string][]float64{
		"sounds/start_listening.pcm": start,
		"sounds/end_listening.pcm":   end,
	} {
		pcm := pcm16(samples)
		if err := os.WriteFile(path, pcm, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "gen_sounds: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s (%d bytes)\n", path, len(pcm))
	}
}
