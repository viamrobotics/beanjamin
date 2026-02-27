// Package speechclient registers the viam-labs:service:speech API and provides
// a client for calling the speech service from Go. It exists because the
// upstream speech-service-api Go package has an incompatible server constructor
// signature with current RDK versions.
package speechclient

import (
	"context"

	pb "github.com/viam-labs/speech-service-api/src/speech_service_api_go/grpc"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot"
	"go.viam.com/utils/rpc"
)

// API is the viam-labs:service:speech resource API.
var API = resource.APINamespace("viam-labs").WithServiceType("speech")

// Named returns the resource name for a speech service with the given name.
func Named(name string) resource.Name {
	return resource.NewName(API, name)
}

// FromRobot gets the named speech service from the robot.
func FromRobot(r robot.Robot, name string) (Speech, error) {
	return robot.ResourceFromRobot[Speech](r, Named(name))
}

// Speech defines the client interface (subset of the full API).
type Speech interface {
	resource.Resource
	Say(ctx context.Context, text string, blocking bool) (string, error)
}

func init() {
	resource.RegisterAPI(API, resource.APIRegistration[Speech]{
		RPCServiceServerConstructor: func(_ resource.APIResourceGetter[Speech], _ logging.Logger) interface{} {
			return &pb.UnimplementedSpeechServiceServer{}
		},
		RPCServiceHandler: pb.RegisterSpeechServiceHandlerFromEndpoint,
		RPCServiceDesc:    &pb.SpeechService_ServiceDesc,
		RPCClient: func(
			ctx context.Context,
			conn rpc.ClientConn,
			remoteName string,
			name resource.Name,
			logger logging.Logger,
		) (Speech, error) {
			return &client{
				Named:  name.PrependRemote(remoteName).AsNamed(),
				client: pb.NewSpeechServiceClient(conn),
				name:   name.ShortName(),
			}, nil
		},
	})
}

type client struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable
	client pb.SpeechServiceClient
	name   string
}

func (c *client) Say(ctx context.Context, text string, blocking bool) (string, error) {
	resp, err := c.client.Say(ctx, &pb.SayRequest{
		Name:     c.name,
		Text:     text,
		Blocking: blocking,
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}
