package main

import (
	"beanjamin"
	"beanjamin/multiposesexecutionswitch"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: beanjamin.Coffee},
		resource.APIModel{API: toggleswitch.API, Model: multiposesexecutionswitch.Model},
	)
}
