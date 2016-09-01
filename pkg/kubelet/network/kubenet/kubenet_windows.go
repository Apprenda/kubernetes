// +build windows

/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubenet

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"

	"github.com/Microsoft/hcsshim"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/apis/componentconfig"
	"k8s.io/kubernetes/pkg/kubelet/network"
)

const (
	kubenetPluginName    = "kubenet"
	containerNetworkType = "Transparent"
	kubenetNetworkName   = "kubenet"
)

type kubenetNetworkPlugin struct {
	network.NoopNetworkPlugin
	host network.Host

	networkInterface string
}

// NewPlugin returns an initialized network plugin
func NewPlugin(networkPluginDir string) network.NetworkPlugin {
	return &kubenetNetworkPlugin{}
}

func (p *kubenetNetworkPlugin) Init(host network.Host, hairpinMode componentconfig.HairpinMode, nonMasqueradeCIDR string) error {
	glog.V(5).Infof("Initialized kubenet plugin")
	p.host = host

	p.networkInterface = os.Getenv("CONTAINER_NETWORK_IFACE")
	if p.networkInterface == "" {
		return errors.New("the CONTAINER_NETWORK_IFACE environment variable is required for Kubenet on Windows. This is the interface that will be used for setting up the container network.")
	}

	return nil
}

func (p *kubenetNetworkPlugin) Name() string {
	return kubenetPluginName
}

// Event will handle the case where the pod CIDR changes, and will setup the container network using the CIDR assigned to the node.
// In the case where the container network already exists, we will validate the network interface being used and move along.
func (p *kubenetNetworkPlugin) Event(name string, details map[string]interface{}) {
	// We only care about POD_CIDR_CHANGE event
	if name != network.NET_PLUGIN_EVENT_POD_CIDR_CHANGE {
		return
	}

	podCIDR, ok := details[network.NET_PLUGIN_EVENT_POD_CIDR_CHANGE_DETAIL_CIDR].(string)
	if !ok {
		glog.Warningf("%s event didn't contain pod CIDR", network.NET_PLUGIN_EVENT_POD_CIDR_CHANGE)
		return
	}

	nets, err := hcsshim.HNSListNetworkRequest("GET", "", "")
	if err != nil {
		glog.Errorf("Error getting list of container networks: %v", err)
	}

	// Check if the network already exists
	for _, n := range nets {
		if n.Name == kubenetNetworkName {
			// validate
			if n.NetworkAdapterName != p.networkInterface {
				glog.Warningf("The container network %q already exists, but it is using an unexpected network adapter %q. Expected network adapter %q.", n.Name, n.NetworkAdapterName, p.networkInterface)
				return
			}

			glog.Infof("Container network %q already exists on the %q network adapter. Skipping configuration.", kubenetNetworkName, n.NetworkAdapterName)
			return
		}
	}

	ip, cidr, err := net.ParseCIDR(podCIDR)
	if err != nil {
		glog.Errorf("Failed to parse CIDR from: %s", podCIDR)
		return
	}

	// Set gateway IP to the first address in the CIDR
	gw := ip.To4()
	gw[3]++

	s := hcsshim.Subnet{
		AddressPrefix:  cidr.String(),
		GatewayAddress: gw.String(),
	}

	net := &hcsshim.HNSNetwork{
		Name:               kubenetNetworkName,
		Type:               containerNetworkType,
		Subnets:            []hcsshim.Subnet{s},
		NetworkAdapterName: p.networkInterface,
	}

	config, err := json.Marshal(net)
	if err != nil {
		glog.Errorf("Failed to marshal HNS Network config: %v", err)
		return
	}

	glog.Infof("Issuing HNS request to create new network: %+v", net)
	hnsresp, err := hcsshim.HNSNetworkRequest("POST", "", string(config))
	if err != nil {
		glog.Errorf("Error creating HNS network: %v", err)
		return
	}

	glog.Infof("HNS Network created: %+v", hnsresp)

	// Restart container runtime to pick up new network
	glog.Info("Restarting container runtime")
	cmd := exec.Command("powershell", "restart-service", "docker")
	out, err := cmd.CombinedOutput()
	if err != nil {
		glog.Errorf("Error restarting docker: output: %s: error: %v", string(out), err)
		return
	}

	// We need to set the transparent network adapter's IP address to be the gateway IP address.
	// This is tricky. We are currently assuming that the network adapter used in the tranparent network will always have the same name.
	prefixLength, _ := cidr.Mask.Size()
	transpIface := `"vEthernet (HNSTransparent)"`
	cmd = exec.Command("powershell", "New-NetIPAddress", "-InterfaceAlias", transpIface, "-IPAddress", gw.String(), "-PrefixLength", strconv.Itoa(prefixLength))
	out, err = cmd.CombinedOutput()
	if err != nil {
		glog.Errorf("Error setting IP address on interface %q: output: %s: error: %v", transpIface, string(out), err)
		return
	}

	glog.Infof("Successfully set the IP address of %s to %s", transpIface, gw.String())
}
