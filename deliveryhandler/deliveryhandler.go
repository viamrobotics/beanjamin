// Package deliveryhandler registers a viam:beanjamin:delivery-handler model
// that implements the rdk:service:generic API. It is the receiving end of
// the coffee service's one-way delivery messaging channel: a hardware-free,
// config-free service a separate machine (e.g. a delivery robot) runs so the
// coffee machine has something to notify. Each message from the coffee
// service's send_delivery_message arrives as a receive_message DoCommand.
// Only the transport exists so far — reacting to the real delivery
// vocabulary (drink_ready, …) will build on it.
package deliveryhandler

import (
	"context"
	"fmt"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

// Model is the full model triplet for this service.
var Model = resource.NewModel("viam", "beanjamin", "delivery-handler")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newDeliveryHandler,
		},
	)
}

// Config is empty — the handler needs no attributes.
type Config struct{}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	return nil, nil, nil
}

type deliveryHandler struct {
	resource.AlwaysRebuild
	name   resource.Name
	logger logging.Logger
}

func newDeliveryHandler(
	_ context.Context,
	_ resource.Dependencies,
	rawConf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	return &deliveryHandler{
		name:   rawConf.ResourceName(),
		logger: logger,
	}, nil
}

func (h *deliveryHandler) Name() resource.Name { return h.name }

func (h *deliveryHandler) Close(_ context.Context) error { return nil }

func (h *deliveryHandler) Status(_ context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}

func (h *deliveryHandler) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	if payload, ok := cmd["receive_message"]; ok {
		return h.receiveMessage(payload)
	}
	err := fmt.Errorf("unknown command, supported commands: receive_message")
	h.logger.Warnw("DoCommand", "error", err)
	return nil, err
}

// receiveMessage handles an inbound receive_message DoCommand from the
// coffee machine. It only logs and acknowledges — message-specific behavior
// (e.g. starting a delivery run on drink_ready) comes later.
func (h *deliveryHandler) receiveMessage(payload any) (map[string]any, error) {
	h.logger.Infof("received peer message: %v", payload)
	return map[string]any{
		"received": true,
		"message":  payload,
	}, nil
}
