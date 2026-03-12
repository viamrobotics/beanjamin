// Minimal module entrypoint for the beanjamin web app.
// Registers a generic component so RDK can manage the module lifecycle.
// The actual web app is served by the Viam platform via the applications config.
package main

import (
	"context"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
)

var model = resource.NewModel("viam", "beanjamin-app", "barista-bot")

type placeholder struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable
	name resource.Name
}

func (p *placeholder) Name() resource.Name { return p.name }

func (p *placeholder) DoCommand(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func init() {
	resource.RegisterComponent(generic.API, model,
		resource.Registration[resource.Resource, resource.NoNativeConfig]{
			Constructor: func(
				_ context.Context,
				_ resource.Dependencies,
				conf resource.Config,
				_ logging.Logger,
			) (resource.Resource, error) {
				return &placeholder{name: conf.ResourceName()}, nil
			},
		},
	)
}

func main() {
	module.ModularMain(resource.APIModel{API: generic.API, Model: model})
}
