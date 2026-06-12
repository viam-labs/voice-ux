// Command gen_sounds regenerates the embedded default earcons as raw PCM16
// (16 kHz mono, little-endian).
//
// The two cues follow the convention shared by Alexa, Google Assistant,
// Mycroft/OVOS and wyoming-satellite: a mirrored two-tone pair, ascending
// for "started listening" and descending for "done listening". Short fades
// avoid clicks.
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
	amplitude  = 0.35
	fadeSec    = 0.008
)

func tone(freqHz, durSec float64) []float64 {
	n := int(sampleRate * durSec)
	fadeN := int(sampleRate * fadeSec)
	out := make([]float64, n)
	for i := range out {
		env := 1.0
		if i < fadeN {
			env = float64(i) / float64(fadeN)
		} else if i > n-fadeN {
			env = float64(n-i) / float64(fadeN)
		}
		out[i] = amplitude * env * math.Sin(2*math.Pi*freqHz*float64(i)/sampleRate)
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
	gap := make([]float64, sampleRate*20/1000) // 20ms silence between tones

	var start []float64
	start = append(start, tone(880.0, 0.08)...) // A5
	start = append(start, gap...)
	start = append(start, tone(1318.5, 0.12)...) // E6

	var end []float64
	end = append(end, tone(1318.5, 0.08)...)
	end = append(end, gap...)
	end = append(end, tone(880.0, 0.12)...)

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
