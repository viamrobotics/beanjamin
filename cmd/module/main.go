package main

import (
	"beanjamin"
	"beanjamin/customerdetector"
	"beanjamin/dialcontrolmotion"
	"beanjamin/multiposesexecutionswitch"
	"beanjamin/texttospeech"

	"go.viam.com/rdk/components/sensor"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: beanjamin.Coffee},
		resource.APIModel{API: toggleswitch.API, Model: multiposesexecutionswitch.Model},
		resource.APIModel{API: generic.API, Model: texttospeech.Model},
		resource.APIModel{API: sensor.API, Model: beanjamin.MaintenanceSensor},
		resource.APIModel{API: generic.API, Model: dialcontrolmotion.Model},
		resource.APIModel{API: generic.API, Model: customerdetector.Model},
	)
}
