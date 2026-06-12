package main

import (
	"voiceux"

	"go.viam.com/rdk/components/audioout"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: audioout.API, Model: voiceux.WakeChimeSpeaker},
	)
}
