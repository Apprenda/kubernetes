package dockertools

import (
	"fmt"
	"io/ioutil"
	"os"
	"time"

	dockertypes "github.com/docker/engine-api/types"
	"github.com/golang/glog"
	cadvisorapi "github.com/google/cadvisor/info/v1"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/record"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/network"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	kubetypes "k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/flowcontrol"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/util/procfs"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
)

// WindowsDockerManager is a docker manager that works with Docker on Windows.
type WindowsDockerManager struct {
	*DockerManager
}

// NewWindowsDockerManager returns a docker manager that is used when the kubelet
// is running on Windows.
func NewWindowsDockerManager(
	client DockerInterface,
	recorder record.EventRecorder,
	livenessManager proberesults.Manager,
	containerRefManager *kubecontainer.RefManager,
	podGetter podGetter,
	machineInfo *cadvisorapi.MachineInfo,
	podInfraContainerImage string,
	qps float32,
	burst int,
	containerLogsDir string,
	osInterface kubecontainer.OSInterface,
	networkPlugin network.NetworkPlugin,
	runtimeHelper kubecontainer.RuntimeHelper,
	httpClient types.HttpGetter,
	execHandler ExecHandler,
	oomAdjuster *oom.OOMAdjuster,
	procFs procfs.ProcFSInterface,
	cpuCFSQuota bool,
	imageBackOff *flowcontrol.Backoff,
	serializeImagePulls bool,
	enableCustomMetrics bool,
	hairpinMode bool,
	seccompProfileRoot string,
	options ...kubecontainer.Option) *WindowsDockerManager {
	dockerManager := NewDockerManager(client,
		recorder,
		livenessManager,
		containerRefManager,
		podGetter,
		machineInfo,
		podInfraContainerImage,
		qps,
		burst,
		containerLogsDir,
		osInterface,
		networkPlugin,
		runtimeHelper,
		httpClient,
		execHandler,
		oomAdjuster,
		procFs,
		cpuCFSQuota,
		imageBackOff,
		serializeImagePulls,
		enableCustomMetrics,
		hairpinMode,
		seccompProfileRoot,
		options...)

	return &WindowsDockerManager{
		DockerManager: dockerManager,
	}
}

// SyncPod syncs the running pod to match the specified desired pod. Due to limitations in Windows containers,
// the infrastructure pod is not used here.
func (dm *WindowsDockerManager) SyncPod(pod *api.Pod, _ api.PodStatus, podStatus *kubecontainer.PodStatus, pullSecrets []api.Secret, backOff *flowcontrol.Backoff) (result kubecontainer.PodSyncResult) {
	start := time.Now()
	defer func() {
		metrics.ContainerManagerLatency.WithLabelValues("SyncPod").Observe(metrics.SinceInMicroseconds(start))
	}()

	containerChanges, err := dm.computePodContainerChanges(pod, podStatus)
	if err != nil {
		result.Fail(err)
		return
	}
	glog.V(3).Infof("Got container changes for pod %q: %+v", format.Pod(pod), containerChanges)

	// Kill any running containers in this pod which are not specified as ones to keep.
	runningContainerStatues := podStatus.GetRunningContainerStatuses()
	for _, containerStatus := range runningContainerStatues {
		_, keep := containerChanges.ContainersToKeep[kubecontainer.DockerID(containerStatus.ID.ID)]
		_, keepInit := containerChanges.InitContainersToKeep[kubecontainer.DockerID(containerStatus.ID.ID)]
		if !keep && !keepInit {
			glog.V(3).Infof("Killing unwanted container %q(id=%q) for pod %q", containerStatus.Name, containerStatus.ID, format.Pod(pod))
			// attempt to find the appropriate container policy
			var podContainer *api.Container
			var killMessage string
			for i, c := range pod.Spec.Containers {
				if c.Name == containerStatus.Name {
					podContainer = &pod.Spec.Containers[i]
					killMessage = containerChanges.ContainersToStart[i]
					break
				}
			}
			killContainerResult := kubecontainer.NewSyncResult(kubecontainer.KillContainer, containerStatus.Name)
			result.AddSyncResult(killContainerResult)
			if err := dm.KillContainerInPod(containerStatus.ID, podContainer, pod, killMessage, nil); err != nil {
				killContainerResult.Fail(kubecontainer.ErrKillContainer, err.Error())
				glog.Errorf("Error killing container %q(id=%q) for pod %q: %v", containerStatus.Name, containerStatus.ID, format.Pod(pod), err)
				return
			}
		}
	}

	// Keep terminated init containers fairly aggressively controlled
	dm.pruneInitContainersBeforeStart(pod, podStatus, containerChanges.InitContainersToKeep)

	// We pass the value of the podIP down to runContainerInPod, which in turn
	// passes it to various other functions, in order to facilitate
	// functionality that requires this value (hosts file and downward API)
	// and avoid races determining the pod IP in cases where a container
	// requires restart but the podIP isn't in the status manager yet.
	//
	// We default to the IP in the passed-in pod status, and overwrite it if the
	// infra container needs to be (re)started.
	podIP := ""
	if podStatus != nil {
		podIP = podStatus.IP
	}

	podInfraContainerID := containerChanges.InfraContainerId

	next, status, done := findActiveInitContainer(pod, podStatus)
	if status != nil {
		if status.ExitCode != 0 {
			// container initialization has failed, flag the pod as failed
			initContainerResult := kubecontainer.NewSyncResult(kubecontainer.InitContainer, status.Name)
			initContainerResult.Fail(kubecontainer.ErrRunInitContainer, fmt.Sprintf("init container %q exited with %d", status.Name, status.ExitCode))
			result.AddSyncResult(initContainerResult)
			if pod.Spec.RestartPolicy == api.RestartPolicyNever {
				utilruntime.HandleError(fmt.Errorf("error running pod %q init container %q, restart=Never: %#v", format.Pod(pod), status.Name, status))
				return
			}
			utilruntime.HandleError(fmt.Errorf("Error running pod %q init container %q, restarting: %#v", format.Pod(pod), status.Name, status))
		}
	}

	// Note: when configuring the pod's containers anything that can be configured by pointing
	// to the namespace of the infra container should use namespaceMode.  This includes things like the net namespace
	// and IPC namespace.  PID mode cannot point to another container right now.
	// See createPodInfraContainer for infra container setup.
	namespaceMode := fmt.Sprintf("container:%v", podInfraContainerID)
	pidMode := getPidMode(pod)

	if next != nil {
		if len(containerChanges.ContainersToStart) == 0 {
			glog.V(4).Infof("No containers to start, stopping at init container %+v in pod %v", next.Name, format.Pod(pod))
			return
		}

		// If we need to start the next container, do so now then exit
		container := next
		startContainerResult := kubecontainer.NewSyncResult(kubecontainer.StartContainer, container.Name)
		result.AddSyncResult(startContainerResult)

		// containerChanges.StartInfraContainer causes the containers to be restarted for config reasons
		if !containerChanges.StartInfraContainer {
			isInBackOff, err, msg := dm.doBackOff(pod, container, podStatus, backOff)
			if isInBackOff {
				startContainerResult.Fail(err, msg)
				glog.V(4).Infof("Backing Off restarting init container %+v in pod %v", container, format.Pod(pod))
				return
			}
		}

		glog.V(4).Infof("Creating init container %+v in pod %v", container, format.Pod(pod))
		if err, msg := dm.tryContainerStart(container, pod, podStatus, pullSecrets, namespaceMode, pidMode, podIP); err != nil {
			startContainerResult.Fail(err, msg)
			utilruntime.HandleError(fmt.Errorf("container start failed: %v: %s", err, msg))
			return
		}

		// Successfully started the container; clear the entry in the failure
		glog.V(4).Infof("Completed init container %q for pod %q", container.Name, format.Pod(pod))
		return
	}
	if !done {
		// init container still running
		glog.V(4).Infof("An init container is still running in pod %v", format.Pod(pod))
		return
	}
	if containerChanges.InitFailed {
		// init container still running
		glog.V(4).Infof("Not all init containers have succeeded for pod %v", format.Pod(pod))
		return
	}

	// Start regular containers
	for idx := range containerChanges.ContainersToStart {
		container := &pod.Spec.Containers[idx]
		startContainerResult := kubecontainer.NewSyncResult(kubecontainer.StartContainer, container.Name)
		result.AddSyncResult(startContainerResult)

		// containerChanges.StartInfraContainer causes the containers to be restarted for config reasons
		if !containerChanges.StartInfraContainer {
			isInBackOff, err, msg := dm.doBackOff(pod, container, podStatus, backOff)
			if isInBackOff {
				startContainerResult.Fail(err, msg)
				glog.V(4).Infof("Backing Off restarting container %+v in pod %v", container, format.Pod(pod))
				continue
			}
		}

		glog.V(4).Infof("Creating container %+v in pod %v", container, format.Pod(pod))
		if err, msg := dm.tryContainerStart(container, pod, podStatus, pullSecrets, namespaceMode, pidMode, podIP); err != nil {
			startContainerResult.Fail(err, msg)
			utilruntime.HandleError(fmt.Errorf("container start failed: %v: %s", err, msg))
			continue
		}
	}
	return
}

func (dm *WindowsDockerManager) computePodContainerChanges(pod *api.Pod, podStatus *kubecontainer.PodStatus) (podContainerChangesSpec, error) {
	start := time.Now()
	defer func() {
		metrics.ContainerManagerLatency.WithLabelValues("computePodContainerChanges").Observe(metrics.SinceInMicroseconds(start))
	}()
	glog.V(5).Infof("Syncing Pod %q: %#v", format.Pod(pod), pod)

	containersToStart := make(map[int]string)
	containersToKeep := make(map[kubecontainer.DockerID]int)

	// check the status of the init containers
	initFailed := false
	initContainersToKeep := make(map[kubecontainer.DockerID]int)

	// keep all successfully completed containers up to and including the first failing container
Containers:
	for i, container := range pod.Spec.InitContainers {
		containerStatus := podStatus.FindContainerStatusByName(container.Name)
		switch {
		case containerStatus == nil:
			continue
		case containerStatus.State == kubecontainer.ContainerStateRunning:
			initContainersToKeep[kubecontainer.DockerID(containerStatus.ID.ID)] = i
		case containerStatus.State == kubecontainer.ContainerStateExited:
			initContainersToKeep[kubecontainer.DockerID(containerStatus.ID.ID)] = i
			// TODO: should we abstract the "did the init container fail" check?
			if containerStatus.ExitCode != 0 {
				initFailed = true
				break Containers
			}
		}
	}

	// check the status of the containers
	for index, container := range pod.Spec.Containers {
		expectedHash := kubecontainer.HashContainer(&container)

		containerStatus := podStatus.FindContainerStatusByName(container.Name)
		if containerStatus == nil || containerStatus.State != kubecontainer.ContainerStateRunning {
			if kubecontainer.ShouldContainerBeRestarted(&container, pod, podStatus) {
				// If we are here it means that the container is dead and should be restarted, or never existed and should
				// be created. We may be inserting this ID again if the container has changed and it has
				// RestartPolicy::Always, but it's not a big deal.
				message := fmt.Sprintf("Container %+v is dead, but RestartPolicy says that we should restart it.", container)
				glog.V(3).Info(message)
				containersToStart[index] = message
			}
			continue
		}

		containerID := kubecontainer.DockerID(containerStatus.ID.ID)
		hash := containerStatus.Hash
		glog.V(3).Infof("pod %q container %q exists as %v", format.Pod(pod), container.Name, containerID)

		if initFailed {
			// initialization failed and Container exists
			// If we have an initialization failure everything will be killed anyway
			// If RestartPolicy is Always or OnFailure we restart containers that were running before we
			// killed them when re-running initialization
			if pod.Spec.RestartPolicy != api.RestartPolicyNever {
				message := fmt.Sprintf("Failed to initialize pod. %q will be restarted.", container.Name)
				glog.V(1).Info(message)
				containersToStart[index] = message
			}
			continue
		}

		// At this point, the container is running
		// We will look for changes and check healthiness for the container.
		containerChanged := hash != 0 && hash != expectedHash
		if containerChanged {
			message := fmt.Sprintf("pod %q container %q hash changed (%d vs %d), it will be killed and re-created.", format.Pod(pod), container.Name, hash, expectedHash)
			glog.Info(message)
			containersToStart[index] = message
			continue
		}

		liveness, found := dm.livenessManager.Get(containerStatus.ID)
		if !found || liveness == proberesults.Success {
			containersToKeep[containerID] = index
			continue
		}
		if pod.Spec.RestartPolicy != api.RestartPolicyNever {
			message := fmt.Sprintf("pod %q container %q is unhealthy, it will be killed and re-created.", format.Pod(pod), container.Name)
			glog.Info(message)
			containersToStart[index] = message
		}
	}

	// Due to the limitations in Windows container, the infrastructure pod is not supported on the Windows side.
	// The main limitation is that there is no support for container networking. This means that each container
	// gets it's own networking stack, making it impossible to share a single IP address.
	createPodInfraContainer := false
	podInfraChanged := false
	podInfraContainerID := kubecontainer.DockerID("")

	return podContainerChangesSpec{
		StartInfraContainer:  createPodInfraContainer,
		InfraChanged:         podInfraChanged,
		InfraContainerId:     podInfraContainerID,
		InitFailed:           initFailed,
		InitContainersToKeep: initContainersToKeep,
		ContainersToStart:    containersToStart,
		ContainersToKeep:     containersToKeep,
	}, nil
}

// tryContainerStart attempts to pull and start the container, returning an error and a reason string if the start
// was not successful.
func (dm *WindowsDockerManager) tryContainerStart(container *api.Container, pod *api.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []api.Secret, namespaceMode, pidMode, podIP string) (err error, reason string) {
	err, msg := dm.imagePuller.EnsureImageExists(pod, container, pullSecrets)
	if err != nil {
		return err, msg
	}

	if container.SecurityContext != nil && container.SecurityContext.RunAsNonRoot != nil && *container.SecurityContext.RunAsNonRoot {
		err := dm.verifyNonRoot(container)
		if err != nil {
			return kubecontainer.ErrVerifyNonRoot, err.Error()
		}
	}

	// For a new container, the RestartCount should be 0
	restartCount := 0
	containerStatus := podStatus.FindContainerStatusByName(container.Name)
	if containerStatus != nil {
		restartCount = containerStatus.RestartCount + 1
	}

	netMode := os.Getenv("CONTAINER_NETWORK")
	_, err = dm.runContainerInPod(pod, container, netMode, namespaceMode, pidMode, podIP, restartCount)
	if err != nil {
		// TODO(bburns) : Perhaps blacklist a container after N failures?
		return kubecontainer.ErrRunContainer, err.Error()
	}
	return nil, ""
}

// determineContainerIP determines the IP address of the given container.  It is expected
// that the container passed is the infrastructure container of a pod and the responsibility
// of the caller to ensure that the correct container is passed.
func (dm *WindowsDockerManager) determineContainerIP(podNamespace, podName string, container *dockertypes.ContainerJSON) string {
	result := ""

	if container.NetworkSettings != nil {
		result = container.NetworkSettings.IPAddress

		if len(result) == 0 {
			for _, network := range container.NetworkSettings.Networks {
				if len(network.IPAddress) > 0 {
					result = network.IPAddress
					break
				}
			}
		}
	}

	if dm.networkPlugin.Name() != network.DefaultPluginName {
		netStatus, err := dm.networkPlugin.GetPodNetworkStatus(podNamespace, podName, kubecontainer.DockerID(container.ID).ContainerID())
		if err != nil {
			glog.Errorf("NetworkPlugin %s failed on the status hook for pod '%s' - %v", dm.networkPlugin.Name(), podName, err)
		} else if netStatus != nil {
			result = netStatus.IP.String()
		}
	}

	return result
}

func (dm *WindowsDockerManager) inspectContainer(id string, podName, podNamespace string) (*kubecontainer.ContainerStatus, string, error) {
	var ip string
	iResult, err := dm.client.InspectContainer(id)
	if err != nil {
		return nil, ip, err
	}
	glog.V(4).Infof("Container inspect result: %+v", *iResult)

	// TODO: Get k8s container name by parsing the docker name. This will be
	// replaced by checking docker labels eventually.
	dockerName, hash, err := ParseDockerName(iResult.Name)
	if err != nil {
		return nil, ip, fmt.Errorf("Unable to parse docker name %q", iResult.Name)
	}
	containerName := dockerName.ContainerName

	var containerInfo *labelledContainerInfo
	containerInfo = getContainerInfoFromLabel(iResult.Config.Labels)

	parseTimestampError := func(label, s string) {
		glog.Errorf("Failed to parse %q timestamp %q for container %q of pod %q", label, s, id, kubecontainer.BuildPodFullName(podName, podNamespace))
	}
	var createdAt, startedAt, finishedAt time.Time
	if createdAt, err = ParseDockerTimestamp(iResult.Created); err != nil {
		parseTimestampError("Created", iResult.Created)
	}
	if startedAt, err = ParseDockerTimestamp(iResult.State.StartedAt); err != nil {
		parseTimestampError("StartedAt", iResult.State.StartedAt)
	}
	if finishedAt, err = ParseDockerTimestamp(iResult.State.FinishedAt); err != nil {
		parseTimestampError("FinishedAt", iResult.State.FinishedAt)
	}

	status := kubecontainer.ContainerStatus{
		Name:         containerName,
		RestartCount: containerInfo.RestartCount,
		Image:        iResult.Config.Image,
		ImageID:      DockerPrefix + iResult.Image,
		ID:           kubecontainer.DockerID(id).ContainerID(),
		ExitCode:     iResult.State.ExitCode,
		CreatedAt:    createdAt,
		Hash:         hash,
	}
	if iResult.State.Running {
		// Container that are running, restarting and paused
		status.State = kubecontainer.ContainerStateRunning
		status.StartedAt = startedAt

		// No infra container on Windows, we get the IP of the container
		ip = dm.determineContainerIP(podNamespace, podName, iResult)
		return &status, ip, nil
	}

	// Find containers that have exited or failed to start.
	if !finishedAt.IsZero() || iResult.State.ExitCode != 0 {
		// Containers that are exited, dead or created (docker failed to start container)
		// When a container fails to start State.ExitCode is non-zero, FinishedAt and StartedAt are both zero
		reason := ""
		message := iResult.State.Error

		// Note: An application might handle OOMKilled gracefully.
		// In that case, the container is oom killed, but the exit
		// code could be 0.
		if iResult.State.OOMKilled {
			reason = "OOMKilled"
		} else if iResult.State.ExitCode == 0 {
			reason = "Completed"
		} else if !finishedAt.IsZero() {
			reason = "Error"
		} else {
			// finishedAt is zero and ExitCode is nonZero occurs when docker fails to start the container
			reason = ErrContainerCannotRun.Error()
			// Adjust time to the time docker attempted to run the container, otherwise startedAt and finishedAt will be set to epoch, which is misleading
			finishedAt = createdAt
			startedAt = createdAt
		}

		terminationMessagePath := containerInfo.TerminationMessagePath
		if terminationMessagePath != "" {
			for _, mount := range iResult.Mounts {
				if mount.Destination == terminationMessagePath {
					path := mount.Source
					if data, err := ioutil.ReadFile(path); err != nil {
						message = fmt.Sprintf("Error on reading termination-log %s: %v", path, err)
					} else {
						message = string(data)
					}
				}
			}
		}
		status.State = kubecontainer.ContainerStateExited
		status.Message = message
		status.Reason = reason
		status.StartedAt = startedAt
		status.FinishedAt = finishedAt
	} else {
		// Non-running containers that are created (not yet started or kubelet failed before calling
		// start container function etc.) Kubelet doesn't handle these scenarios yet.
		status.State = kubecontainer.ContainerStateUnknown
	}
	return &status, "", nil
}

func (dm *WindowsDockerManager) GetPodStatus(uid kubetypes.UID, name, namespace string) (*kubecontainer.PodStatus, error) {
	podStatus := &kubecontainer.PodStatus{ID: uid, Name: name, Namespace: namespace}
	// Now we retain restart count of container as a docker label. Each time a container
	// restarts, pod will read the restart count from the registered dead container, increment
	// it to get the new restart count, and then add a label with the new restart count on
	// the newly started container.
	// However, there are some limitations of this method:
	//	1. When all dead containers were garbage collected, the container status could
	//	not get the historical value and would be *inaccurate*. Fortunately, the chance
	//	is really slim.
	//	2. When working with old version containers which have no restart count label,
	//	we can only assume their restart count is 0.
	// Anyhow, we only promised "best-effort" restart count reporting, we can just ignore
	// these limitations now.
	var containerStatuses []*kubecontainer.ContainerStatus
	// We have added labels like pod name and pod namespace, it seems that we can do filtered list here.
	// However, there may be some old containers without these labels, so at least now we can't do that.
	// TODO(random-liu): Do only one list and pass in the list result in the future
	// TODO(random-liu): Add filter when we are sure that all the containers have the labels
	containers, err := dm.client.ListContainers(dockertypes.ContainerListOptions{All: true})
	if err != nil {
		return podStatus, err
	}
	// Loop through list of running and exited docker containers to construct
	// the statuses. We assume docker returns a list of containers sorted in
	// reverse by time.
	// TODO: optimization: set maximum number of containers per container name to examine.
	for _, c := range containers {
		if len(c.Names) == 0 {
			continue
		}
		dockerName, _, err := ParseDockerName(c.Names[0])
		if err != nil {
			continue
		}
		if dockerName.PodUID != uid {
			continue
		}
		result, ip, err := dm.inspectContainer(c.ID, name, namespace)
		if err != nil {
			if _, ok := err.(containerNotFoundError); ok {
				// https://github.com/kubernetes/kubernetes/issues/22541
				// Sometimes when docker's state is corrupt, a container can be listed
				// but couldn't be inspected. We fake a status for this container so
				// that we can still return a status for the pod to sync.
				result = &kubecontainer.ContainerStatus{
					ID:    kubecontainer.DockerID(c.ID).ContainerID(),
					Name:  dockerName.ContainerName,
					State: kubecontainer.ContainerStateUnknown,
				}
				glog.Errorf("Unable to inspect container %q: %v", c.ID, err)
			} else {
				return podStatus, err
			}
		}
		containerStatuses = append(containerStatuses, result)
		if ip != "" {
			podStatus.IP = ip
		}
	}

	podStatus.ContainerStatuses = containerStatuses
	return podStatus, nil
}
