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
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"

	"github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	dockernetwork "github.com/docker/engine-api/types/network"
	"github.com/golang/glog"
	"golang.org/x/net/context"
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
	dockerClient     *client.Client
}

// NewPlugin returns an initialized network plugin
func NewPlugin(networkPluginDir string) network.NetworkPlugin {
	return &kubenetNetworkPlugin{}
}

func (p *kubenetNetworkPlugin) Init(host network.Host, hairpinMode componentconfig.HairpinMode, nonMasqueradeCIDR string, mtu int) error {
	glog.V(5).Infof("Initialized kubenet plugin")
	p.host = host

	p.networkInterface = os.Getenv("CONTAINER_NETWORK_IFACE")
	if p.networkInterface == "" {
		return errors.New("the CONTAINER_NETWORK_IFACE environment variable is required for Kubenet on Windows. This is the interface that will be used for setting up the container network.")
	}

	c, err := client.NewEnvClient()
	if err != nil {
		return fmt.Errorf("error initializing new docker client: %v", err)
	}
	p.dockerClient = c

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

	ctx := context.Background()

	nets, err := p.dockerClient.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		glog.Errorf("Error listing docker networks: %v", err)
		return
	}

	for _, n := range nets {
		if n.Name == kubenetNetworkName {
			glog.Infof("Network options: %+v", n.Options)
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

	// IPAM configuration inspect
	ipam := dockernetwork.IPAM{
		Driver: "default",
		Config: []dockernetwork.IPAMConfig{{Subnet: cidr.String(), Gateway: gw.String()}},
	}

	netCreate := types.NetworkCreate{
		CheckDuplicate: true,
		Driver:         "transparent",
		IPAM:           ipam,
		Options:        map[string]string{"com.docker.network.windowsshim.interface": p.networkInterface},
	}

	resp, err := p.dockerClient.NetworkCreate(ctx, kubenetNetworkName, netCreate)
	if err != nil {
		glog.Errorf("error creating network for Kubenet: %v", err)
		return
	}
	glog.Infof("successfully created network: %v", resp)

	// We need to set the transparent network adapter's IP address to be the gateway IP address.
	// This is tricky. We are currently assuming that the network adapter used in the tranparent network will always have the same name.
	prefixLength, _ := cidr.Mask.Size()
	transpIface := `"vEthernet (HNSTransparent)"`
	cmd := exec.Command("powershell", "New-NetIPAddress", "-InterfaceAlias", transpIface, "-IPAddress", gw.String(), "-PrefixLength", strconv.Itoa(prefixLength))
	out, err := cmd.CombinedOutput()
	if err != nil {
		glog.Errorf("Error setting IP address on interface %q: output: %s: error: %v", transpIface, string(out), err)
		return
	}

	glog.Infof("Successfully set the IP address of %s to %s", transpIface, gw.String())
}
