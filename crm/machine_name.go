package crm

import (
	"context"
	"sync"
	"time"

	"go.viam.com/rdk/app"
	"go.viam.com/rdk/logging"
)

// machineNameTimeout caps one lookup, so an unreachable app.viam.com delays a
// recap by at most this long.
const machineNameTimeout = 5 * time.Second

// newMachineNameLookup returns a function giving this machine's name as shown
// on app.viam.com. Viam injects no name into modules, only the machine ID and
// a machine-scoped API key, so the name is fetched from the app API. It is
// looked up on first use rather than at construction, so a machine starting
// offline still builds the service, and a failed lookup is retried on the
// next call. A successful one is cached for the life of the service. The
// function returns "" when the name can't be learned.
func newMachineNameLookup(machineID string, logger logging.Logger) func(context.Context) string {
	var (
		mu   sync.Mutex
		name string
	)
	return func(ctx context.Context) string {
		mu.Lock()
		defer mu.Unlock()
		if name != "" || machineID == "" {
			return name
		}
		ctx, cancel := context.WithTimeout(ctx, machineNameTimeout)
		defer cancel()
		client, err := app.CreateViamClientFromEnvVars(ctx, nil, logger)
		if err != nil {
			logger.Warnf("can't reach app.viam.com for the machine name: %v", err)
			return ""
		}
		defer func() {
			if err := client.Close(); err != nil {
				logger.Debugf("closing the app.viam.com client: %v", err)
			}
		}()
		robot, err := client.AppClient().GetRobot(ctx, machineID)
		if err != nil {
			logger.Warnf("looking up the machine name: %v", err)
			return ""
		}
		name = robot.Name
		return name
	}
}
