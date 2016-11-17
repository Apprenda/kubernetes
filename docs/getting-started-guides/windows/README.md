# Running Windows Server Containers using Kubernetes
Windows Server Containers are supported on Kubernetes as an alpha feature. Kubernetes control plane (API Server, Scheduler, Controller Manager etc) continues to run on Linux, kubelet and kube-proxy can be run on Windows.

## Prerequisites
We have tested Windows Server Containers for Kubernetes using the following,

1. Kubernetes control plane running on existing Linux infrastructure
2. Windows Server 2016 RTM
3. Docker Version 1.12.2-cs2-ws-beta

## Networking
An initial approach to Windows networking using L3 routing is proposed. Given that we are working towards an initial POC of Kubernetes on Windows, this seems like the path of least resistance. Instead of trying to use a third-party networking plugin (e.g. flannel, calico, etc) and make it work on Windows, we can rely on existing technology that is built into the windows and linux machines.
In this L3 networking approach, we choose a /16 subnet for the cluster nodes, and we assign a /24 subnet to each worker node. All pods on a given worker node will be connected to the /24 subnet. This allows pods on the same node to communicate with each other, but doesn’t quite solve networking across nodes. In order to enable networking between pods running on different nodes, we use routing features that are built into Windows Server 2016 and Linux.

### Linux
This is already supported on Linux using the cbr0 bridge, which essentially creates a private network local to the node. Similar to the windows side, routes must be created to the other pod CIDRs to send packets via the “public” NIC.

### Windows
We will need the following on each Kubernetes Windows node:

1. Two NICs - The two windows container networking modes we are interested in (transparent and L2 bridge) both use an external hyper-v virtual switch. This means that one of the NICs is lost to the bridge, creating the need for the second one. (Emulating might be possible, but hasn’t been explored)
2. Transparent container network created - As of now, this is done manually, but could potentially be done by the Kubelet. Need more investigation here.
3. RRAS (Routing) Windows feature enabled - Allows routing between NICs on the box, and also “captures” packets that have the destination IP of a pod running on the node.
4. Routes defined pointing to the other pod CIDRs via the “public” NIC - These routes are added to the built-in routing table.

The following diagram attempts to capture Windows Setup
![Windows Setup](windows-setup.png)

## Setup
### Host Setup
#### Windows

1. Windows host running Windows Server 2016 and setup according to instructions [here](https://msdn.microsoft.com/en-us/virtualization/windowscontainers/quick_start/quick_start_windows_server)
2. RRAS (Routing) Windows feature enabled
3. [Install docker](https://msdn.microsoft.com/en-us/virtualization/windowscontainers/quick_start/quick_start_windows_server)

#### Linux

Linux hosts should be setup according to their respective distro documentation. Linux host also require to have CNI Setup.

### Component Setup
In order to build the work node components, the *kubelet* and *kube-proxy* for Windows, the following needs to be installed on the host
* Git, Go 1.7.1+ 
* make (if using Linux or MacOS)
* Important notes and other dependencies are listed [here](https://github.com/kubernetes/kubernetes/blob/master/docs/devel/development.md#building-kubernetes-on-a-local-osshell-environment)

#### kubelet

In order to build the *kubelet*, run:

1. `cd $GOPATH/src/k8s.io/kubernetes`
2. Build *kubelet*
   1. Linux/MacOS: `KUBE_BUILD_PLATFORMS=windows/amd64 make WHAT=cmd/kubelet`
   2. Windows: `go build cmd/kubelet/kubelet.go`

#### kube-proxy

In order to build *kube-proxy*, run:

1. `cd $GOPATH/src/k8s.io/kubernetes`
2. Build *kube-proxy*
   1. Linux/MacOS: `KUBE_BUILD_PLATFORMS=windows/amd64 make WHAT=cmd/kube-proxy`
   2. Windows: `go build cmd/kube-proxy/proxy.go`

### Route Setup

The below setup assumes one Linux and two Windows Server 2016 nodes and a cluster CIDR 192.168.0.0/16

| Hostname | Routable IP address | Pod CIDR |
| --- | --- | --- |
| Lin01 | `<IP of Lin01 host>` | 192.168.0.0/24 |
| Win01 | `<IP of Win01 host>` | 192.168.1.0/24 |
| Win02 | `<IP of Win02 host>` | 192.168.2.0/24 |

**Lin01**
```
ip route add 192.168.1.0/24 via <IP of Win01 host>
ip route add 192.168.2.0/24 via <IP of Win02 host>
```

**Win01**
```
docker network create -d transparent --gateway 192.168.1.1 --subnet 192.168.1.0/24 <network name>
# A bridge is created with Adapter name "vEthernet (HNSTransparent)". Set its IP address to transparent network gateway
netsh interface ipv4 set address "vEthernet (HNSTransparent)" addr=192.168.1.1
route add 192.168.0.0 mask 255.255.255.0 192.168.0.1 if <Interface Id of the Routable Ethernet Adapter> -p
route add 192.168.2.0 mask 255.255.255.0 192.168.2.1 if <Interface Id of the Routable Ethernet Adapter> -p
```

**Win02**
```
docker network create -d transparent --gateway 192.168.2.1 --subnet 192.168.2.0/24 <network name>
# A bridge is created with Adapter name "vEthernet (HNSTransparent)". Set its IP address to transparent network gateway
netsh interface ipv4 set address "vEthernet (HNSTransparent)" addr=192.168.2.1
route add 192.168.0.0 mask 255.255.255.0 192.168.0.1 if <Interface Id of the Routable Ethernet Adapter> -p
route add 192.168.1.0 mask 255.255.255.0 192.168.1.1 if <Interface Id of the Routable Ethernet Adapter> -p
```

## Starting the Cluster
For now, the Kubernetes control plane continues to run on Linux and as a result we cannot have a Windows only Kubernetes Cluster. 
## Linux
Use your preferred method to start Kubernetes cluster on Linux. Please note that Cluster CIDR might need to be updated.
## Windows
### kubelet
Run the following in a powershell window

1. Set environment variable *CONTAINER_NETWORK* value to the docker container network to use
`$env:CONTAINER_NETWORK = "<docker network>"`

2. Run *kubelet* executable using the below command
`kubelet.exe --hostname-override=<ip address/hostname of the windows node>  --pod-infra-container-image="apprenda/pause" --resolv-conf="" --api_servers=<api server location>`

### kube-proxy

Run the following in a powershell admin window (Run as administrator)

1. Set environment variable *INTERFACE_TO_ADD_SERVICE_IP* value to a node only network interface, interface created when docker is installed should work
`$env:INTERFACE_TO_ADD_SERVICE_IP = "vEthernet (HNS Internal NIC)"`

2. Run *kube-proxy* executable using the below command
`.\proxy.exe --v=3 --proxy-mode=userspace --hostname-override=<ip address/hostname of the windows node> --master=<api server location> --bind-address=<ip address of the windows node>`

## Known Limitations:
1. There is no network namespace in Windows and as a result currently only one container per pod is supported
2. Secrets currently do not work because of a bug in Windows Server Containers described [here](https://github.com/docker/docker/issues/28401)
3. ConfigMaps have not been implemented yet.
4. `kube-proxy` implementation uses `netsh portproxy` and as it only supports TCP, DNS currently works only if the client retries DNS query using TCP  

## Notes
1. `apprenda/pause` image can be pulled from `https://hub.docker.com/r/apprenda/pause`
2. DNS support for Windows recently got merged to docker master and is currently not supported in a stable docker release. To use DNS build docker from master or download the binary from [Docker master](https://master.dockerproject.org/) 