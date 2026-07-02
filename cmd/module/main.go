package main

import (
	"beanjamin/coffee"
	"beanjamin/customerdetector"
	"beanjamin/dialcontrolmotion"
	"beanjamin/maintenancesensor"
	"beanjamin/multiposesexecutionswitch"

	"go.viam.com/rdk/components/sensor"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: coffee.Model},
		resource.APIModel{API: toggleswitch.API, Model: multiposesexecutionswitch.Model},
		resource.APIModel{API: sensor.API, Model: maintenancesensor.Model},
		resource.APIModel{API: sensor.API, Model: coffee.OrderSensor},
		resource.APIModel{API: generic.API, Model: dialcontrolmotion.Model},
		resource.APIModel{API: generic.API, Model: customerdetector.Model},
	)
}
