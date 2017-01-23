package spread

import (
	"errors"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/arm/devtestlabs"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/google/uuid"
	"golang.org/x/net/context"
)

const (
	azureClientId = "f8986e48-7aa9-4aa8-9d03-6aad03138954"
)

var (
	azureDefaultPassword = "Spread.42"
	azureDefaultRegion   = "westeurope"
	azureDefaultUsername = "ubuntu"
	azureSystemsMap      = map[string]string{
		"ubuntu-14.04-64": "ubuntu-14-04",
		"ubuntu-16.04-64": "ubuntu-16-04",
	} // We need to map system names here as azure is quite restrictive in terms of naming resources.
)

// Azure returns a Provider implementation connecting to an Azure devtestlabs instance.
//
// The provider requires the following environment variables to be set:
//   * SPREAD_AZURE_SUBSCRIPTION_ID: The Azure subscription ID for accesing the lab.
//   * SPREAD_AZURE_TENANT_ID: The Azure tenant ID for accessing the lab.
//   * SPREAD_AZURE_CLIENT_SECRET: The secret client key.
//   * SPREAD_AZURE_RESOURCE_GROUP: The Azure source group hosting the devtestlabs instance.
//   * SPREAD_AZURE_LAB_NAME: The name of the Azure devtestlabs instance.
//   * SPREAD_AZURE_LAB_VN: The name of the virtualnetwork instance attached to the lab instance.
func Azure(p *Project, b *Backend, o *Options) Provider {
	subscriptionId := os.Getenv("SPREAD_AZURE_SUBSCRIPTION_ID")
	tenantId := os.Getenv("SPREAD_AZURE_TENANT_ID")
	clientSecret := os.Getenv("SPREAD_AZURE_CLIENT_SECRET")
	resourceGroup := os.Getenv("SPREAD_AZURE_RESOURCE_GROUP")
	labName := os.Getenv("SPREAD_AZURE_LAB_NAME")
	virtualNetworkName := os.Getenv("SPREAD_AZURE_LAB_VN")

	if len(subscriptionId) == 0 || len(tenantId) == 0 || len(clientSecret) == 0 {
		return nil
	}

	oauthConfig, err := azure.PublicCloud.OAuthConfigForTenant(tenantId)
	if err != nil {
		return nil
	}

	spt, err := azure.NewServicePrincipalToken(*oauthConfig, azureClientId, clientSecret, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return nil
	}

	fc := devtestlabs.NewFormulaOperationsClient(subscriptionId)
	fc.Authorizer = spt

	lc := devtestlabs.NewLabOperationsClient(subscriptionId)
	lc.Authorizer = spt

	vnc := devtestlabs.NewVirtualNetworkOperationsClient(subscriptionId)
	vnc.Authorizer = spt

	vmc := devtestlabs.NewVirtualMachineClient(subscriptionId)
	vmc.Authorizer = spt

	return &azureProvider{
		project:              p,
		backend:              b,
		options:              o,
		formulaClient:        fc,
		labClient:            lc,
		virtualNetworkClient: vnc,
		virtualMachineClient: vmc,
		resourceGroup:        resourceGroup,
		labName:              labName,
		virtualNetworkName:   virtualNetworkName,
	}
}

type azureProvider struct {
	project              *Project
	backend              *Backend
	options              *Options
	formulaClient        devtestlabs.FormulaOperationsClient
	labClient            devtestlabs.LabOperationsClient
	virtualNetworkClient devtestlabs.VirtualNetworkOperationsClient
	virtualMachineClient devtestlabs.VirtualMachineClient
	resourceGroup        string
	labName              string
	virtualNetworkName   string
}

type azureVm struct {
	Name    string
	Address string
}
type azureServer struct {
	p      *azureProvider
	vm     *azureVm
	system *System
}

func (s *azureServer) String() string {
	return s.system.String()
}

func (s *azureServer) Provider() Provider {
	return s.p
}

func (s *azureServer) Address() string {
	return s.vm.Address
}

func (s *azureServer) System() *System {
	return s.system
}

func (s *azureServer) ReuseData() interface{} {
	return s.vm
}

func (s *azureServer) Discard(ctx context.Context) error {
	_, err := s.p.virtualMachineClient.DeleteResource(s.p.resourceGroup, s.p.labName, s.vm.Name, nil)
	return err
}

func (p *azureProvider) Backend() *Backend {
	return p.backend
}

func (p *azureProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	vm, ok := rsystem.Data.(*azureVm)
	if !ok || vm == nil {
		return nil, errors.New("Missing VM data")
	}
	s := &azureServer{
		p:      p,
		vm:     vm,
		system: system,
	}
	return s, nil
}

func (p *azureProvider) Allocate(ctx context.Context, system *System) (Server, error) {
	formulaName, ok := azureSystemsMap[system.Name]
	if !ok {
		return nil, fmt.Errorf("System %s is not supported", system.Name)
	}

	formula, err := p.formulaClient.GetResource(p.resourceGroup, p.labName, formulaName)
	if err != nil {
		return nil, err
	}

	vn, err := p.virtualNetworkClient.GetResource(p.resourceGroup, p.labName, p.virtualNetworkName)
	if err != nil {
		return nil, err
	}

	ru, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}

	name := ru.String()
	formula.FormulaContent.Name = &name
	formula.FormulaContent.Location = &azureDefaultRegion
	formula.FormulaContent.UserName = &azureDefaultUsername
	formula.FormulaContent.Password = &azureDefaultPassword
	formula.FormulaContent.LabVirtualNetworkID = vn.ID

	// TODO(tvoss): We should leverage expirationDate as soon as it is available via the SDK
	// and correctly support halt-timeout.
	// See https://azure.microsoft.com/en-us/blog/set-expiration-date-for-vms-in-azure-devtest-labs/
	_, err = p.labClient.CreateEnvironment(p.resourceGroup, p.labName, *formula.FormulaContent, nil)
	if err != nil {
		return nil, err
	}

	labVm, err := p.virtualMachineClient.GetResource(p.resourceGroup, p.labName, name)
	if err != nil {
		return nil, err
	}

	system.Username = azureDefaultUsername
	system.Password = azureDefaultPassword

	return &azureServer{p, &azureVm{*labVm.Name, *labVm.Fqdn}, system}, nil
}
