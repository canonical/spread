package spread

import (
	"errors"
	"fmt"
	"github.com/Azure/azure-sdk-for-go/arm/compute"
	"github.com/Azure/azure-sdk-for-go/arm/network"
	"github.com/Azure/azure-sdk-for-go/arm/resources/resources"
	"github.com/Azure/go-autorest/autorest/azure"
	"golang.org/x/net/context"
	"os"
)

const (
	azureClientId                   = "f8986e48-7aa9-4aa8-9d03-6aad03138954"
	azureDefaultUsername            = "ubuntu"
	azureProvisioningStateSucceeded = "ProvisioningState/succeeded"
	azurePowerStateDeallocated      = "PowerState/deallocated"
)

func Azure(p *Project, b *Backend, o *Options) Provider {
	subscriptionId := os.Getenv("SPREAD_AZURE_SUBSCRIPTION_ID")
	tenantId := os.Getenv("SPREAD_AZURE_TENANT_ID")
	clientSecret := os.Getenv("SPREAD_AZURE_CLIENT_SECRET")

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

	vmClient := compute.NewVirtualMachinesClient(subscriptionId)
	vmClient.Authorizer = spt

	itfClient := network.NewInterfacesClient(subscriptionId)
	itfClient.Authorizer = spt

	netClient := network.NewPublicIPAddressesClient(subscriptionId)
	netClient.Authorizer = spt

	resClient := resources.NewClient(subscriptionId)
	resClient.Authorizer = spt

	return &azureProvider{p, b, o, vmClient, itfClient, netClient, resClient}
}

type azureProvider struct {
	project   *Project
	backend   *Backend
	options   *Options
	vmClient  compute.VirtualMachinesClient
	itfClient network.InterfacesClient
	netClient network.PublicIPAddressesClient
	resClient resources.Client
}

type azureServer struct {
	p *azureProvider

	vm      *compute.VirtualMachine
	system  *System
	address string
}

func (s *azureServer) String() string {
	return s.system.String()
}

func (s *azureServer) Provider() Provider {
	return s.p
}

func (s *azureServer) Address() string {
	return s.address
}

func (s *azureServer) System() *System {
	return s.system
}

func (s *azureServer) ReuseData() interface{} {
	return nil
}

func (s *azureServer) Discard(ctx context.Context) error {
	_, err := s.p.vmClient.Deallocate(s.system.Name, *s.vm.Name, nil)
	return err
}

func (p *azureProvider) Backend() *Backend {
	return p.backend
}

func (p *azureProvider) Reuse(ctx context.Context, rsystem *ReuseSystem, system *System) (Server, error) {
	s := &azureServer{
		p:       p,
		system:  system,
		address: rsystem.Address,
	}
	return s, nil
}

func (p *azureProvider) Allocate(ctx context.Context, system *System) (Server, error) {
	fmt.Println(system.Name)

	vm, err := p.selectVirtualMachine(system.Name)
	if err != nil {
		return nil, err
	}

	_, err = p.vmClient.Start(system.Name, *vm.Name, nil)
	if err != nil {
		return nil, err
	}

	nis := vm.VirtualMachineProperties.NetworkProfile.NetworkInterfaces

	var primaryInterfaceId *string
	if len(*nis) == 1 {
		primaryInterfaceId = (*nis)[0].ID
	} else {
		for _, ni := range *nis {
			if *ni.NetworkInterfaceReferenceProperties.Primary {
				primaryInterfaceId = ni.ID
			}
		}
	}

	if primaryInterfaceId == nil {
		return nil, errors.New("No primary network interface")
	}

	resource, err := p.resClient.GetByID(*primaryInterfaceId)
	if err != nil {
		return nil, err
	}

	itf, err := p.itfClient.Get(system.Name, *resource.Name, "")
	if err != nil {
		return nil, err
	}

	var publicIP *network.PublicIPAddress
	for _, ipConfig := range *itf.IPConfigurations {
		if ipConfig.PublicIPAddress != nil {
			publicIP = ipConfig.PublicIPAddress
		}
	}

	if publicIP == nil {
		return nil, errors.New("No public IP for VM")
	}

	resource, err = p.resClient.GetByID(*publicIP.ID)
	if err != nil {
		return nil, err
	}

	ip, err := p.netClient.Get(system.Name, *resource.Name, "")
	if err != nil {
		return nil, err
	}

	system.Username = azureDefaultUsername
	system.Password = system.Name

	return &azureServer{p, vm, system, *ip.IPAddress}, nil
}

func (p *azureProvider) selectVirtualMachine(resourceGroup string) (*compute.VirtualMachine, error) {
	result, err := p.vmClient.List(resourceGroup)
	if err != nil {
		return nil, err
	}
	// Walk through all known vms in the given resource group and find
	// one that is in PowerState/dellocated. We should randomize the selection
	// process here and minimize clashes with other spread instances selecting
	// vm instances.
	var finalVm *compute.VirtualMachine
	for {
		for _, vm := range *result.Value {
			iv, err := p.vmClient.Get(resourceGroup, *vm.Name, compute.InstanceView)
			if err != nil {
				continue
			}

			provisionedSuccessfully := false
			deallocated := false
			for _, status := range *iv.InstanceView.Statuses {
				switch *status.Code {
				case azureProvisioningStateSucceeded:
					provisionedSuccessfully = true
					break
				case azurePowerStateDeallocated:
					deallocated = true
					break
				}

				if provisionedSuccessfully && deallocated {
					finalVm = &iv
					break
				}
			}
		}

		if finalVm != nil {
			return finalVm, nil
		}

		if result.NextLink == nil {
			break
		}
	}
	return nil, errors.New("No VM available")
}
