package cloud

import (
	"github.com/ergo-services/ergo/etf"
	"github.com/ergo-services/ergo/gen"
	"github.com/ergo-services/ergo/lib"
	"github.com/ergo-services/ergo/node"
)

type CloudApp struct {
	gen.Application
	options node.CloudOptions
}

func CreateApp(options node.CloudOptions) gen.ApplicationBehavior {
	return &CloudApp{
		options: options,
	}
}

func (ca *CloudApp) Load(args ...etf.Term) (gen.ApplicationSpec, error) {
	return gen.ApplicationSpec{
		Name:        "cloud_app",
		Description: "Ergo Cloud Support Application",
		Version:     "v.1.0",
		Children: []gen.ApplicationChildSpec{
			gen.ApplicationChildSpec{
				Child: &cloudAppSup{},
				Name:  "cloud_app_sup",
			},
		},
	}, nil
}

func (ca *CloudApp) Start(p gen.Process, args ...etf.Term) {
	// add static route with custom handshake
	// cloudHandshake = CreateCloudHandshake()
	// node.AddStaticRoute("cloud.ergo.services", node.StaticRouteOptions)
}

type cloudAppSup struct {
	gen.Supervisor
}

func (cas *cloudAppSup) Init(args ...etf.Term) (gen.SupervisorSpec, error) {
	return gen.SupervisorSpec{
		Children: []gen.SupervisorChildSpec{
			gen.SupervisorChildSpec{
				Name:  "cloud_client",
				Child: &cloudClient{},
			},
		},
		Strategy: gen.SupervisorStrategy{
			Type:      gen.SupervisorStrategyOneForOne,
			Intensity: 10,
			Period:    5,
			Restart:   gen.SupervisorStrategyRestartPermanent,
		},
	}, nil
}

type cloudClient struct {
	gen.Server
}

func (cc *cloudClient) Init(process *gen.ServerProcess, args ...etf.Term) error {
	lib.Log("CLOUD_CLIENT: Init: %#v", args)
	// initiate connection with the cloud
	return nil
}

func (cc *cloudClient) HandleCall(process *gen.ServerProcess, from gen.ServerFrom, message etf.Term) (etf.Term, gen.ServerStatus) {
	lib.Log("CLOUD_CLIENT: HandleCall: %#v, From: %#v", message, from)
	return nil, gen.ServerStatusOK
}

func (cc *cloudClient) HandleCast(process *gen.ServerProcess, message etf.Term) gen.ServerStatus {
	lib.Log("CLOUD_CLIENT: HandleCast: %#v", message)
	return gen.ServerStatusOK
}

func (cc *cloudClient) HandleInfo(process *gen.ServerProcess, message etf.Term) gen.ServerStatus {
	lib.Log("CLOUD_CLIENT: HandleInfo: %#v", message)
	return gen.ServerStatusOK
}
func (cc *cloudClient) Terminate(process *gen.ServerProcess, reason string) {
	lib.Log("CLOUD_CLIENT: Terminated with reason: %v", reason)
	return
}
