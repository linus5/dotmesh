package main

import (
	"flag"
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	//"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	lister_v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// Log verbosities:

// -v3 = log every raw watch event
// -v2 = log relevant status of pods and nodes
// -v1 = log summary of cluster checks

// --- PLEASE NOTE --- PLEASE NOTE ---

// There should be NO persistent state stored in this thing other than
// caches and other recreatable stuff.

// We want it to be that stopping and starting the operator has no
// practical effects at all.

// The easiest way to do this is to prohibit meaningful state inside
// the operator process :-)

const DOTMESH_NAMESPACE = "dotmesh"
const DOTMESH_CONFIG_MAP = "configuration"
const DOTMESH_NODE_LABEL = "dotmesh.io/node"
const DOTMESH_POD_ROLE_LABEL = "dotmesh.io/role"
const DOTMESH_ROLE_SERVER = "dotmesh-server"

// ConfigMap keys

const CONFIG_NODE_SELECTOR = "nodeSelector"
const CONFIG_UPGRADES_URL = "upgradesUrl"
const CONFIG_UPGRADES_INTERVAL_SECONDS = "upgradesIntervalSeconds"
const CONFIG_FLEXVOLUME_DRIVER_DIR = "flexvolumeDriverDir"
const CONFIG_POOL_NAME = "poolName"
const CONFIG_LOG_ADDRESS = "logAddress"
const CONFIG_MODE = "storageMode"

const CONFIG_MODE_LOCAL = "local" // Value for CONFIG_MODE
const CONFIG_LOCAL_POOL_SIZE_PER_NODE = "local.poolSizePerNode"
const CONFIG_LOCAL_POOL_LOCATION = "local.poolLocation"

// These values are fed in via the build system at link time
var DOTMESH_VERSION string
var DOTMESH_IMAGE string

const CLUSTER_MINIMUM_RATIO = 0.75 // When restarting pods, try to keep at least this many of the nodes running a pod.

func GetClientConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

var Version string = "none" // Set at compile time

func main() {
	// When running as a pod in-cluster, a kubeconfig is not needed. Instead
	// this will make use of the service account injected into the pod.
	// However, allow the use of a local kubeconfig as this can make local
	// development & testing easier.
	kubeconfig := flag.String("kubeconfig", "", "Path to a kubeconfig file")

	// We log to stderr because glog will default to logging to a file.
	// By setting this debugging is easier via `kubectl logs`
	flag.Set("logtostderr", "true")
	flag.Parse()

	// Build the client config - optionally using a provided kubeconfig file.
	config, err := GetClientConfig(*kubeconfig)
	if err != nil {
		glog.Fatalf("Failed to load client config: %v", err)
	}
	// Construct the Kubernetes client
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create kubernetes client: %v", err)
	}

	stopCh := make(chan struct{})
	defer close(stopCh)

	newDotmeshController(client).Run(stopCh)
}

type dotmeshController struct {
	client       kubernetes.Interface
	nodeLister   lister_v1.NodeLister
	podLister    lister_v1.PodLister
	nodeInformer cache.Controller
	podInformer  cache.Controller

	updatesNeeded     bool
	updatesNeededLock *sync.Mutex

	config *v1.ConfigMap
}

func provideDefault(m *map[string]string, key string, deflt string) {
	_, ok := (*m)[key]
	if !ok {
		glog.V(1).Infof("Config variable %s not specified, defaulting to '%s'", key, deflt)
		(*m)[key] = deflt
	} else {
		glog.V(1).Infof("Config variable %s set to '%s'", key, (*m)[key])
	}
}

func newDotmeshController(client kubernetes.Interface) *dotmeshController {
	rc := &dotmeshController{
		client:            client,
		updatesNeeded:     false,
		updatesNeededLock: &sync.Mutex{},
	}

	config, err := client.Core().ConfigMaps(DOTMESH_NAMESPACE).Get(DOTMESH_CONFIG_MAP, meta_v1.GetOptions{})

	if err != nil {
		glog.Infof("Error fetching configmap %s/%s: %+v, using defaults", DOTMESH_NAMESPACE, DOTMESH_CONFIG_MAP, err)
		rc.config = &v1.ConfigMap{Data: map[string]string{}}
	} else {
		rc.config = config.DeepCopy()
	}

	// Fill in defaults
	provideDefault(&rc.config.Data, CONFIG_NODE_SELECTOR, "")
	provideDefault(&rc.config.Data, CONFIG_UPGRADES_URL, "https://checkpoint.dotmesh.com/")
	provideDefault(&rc.config.Data, CONFIG_UPGRADES_INTERVAL_SECONDS, "14400")
	provideDefault(&rc.config.Data, CONFIG_FLEXVOLUME_DRIVER_DIR, "/usr/libexec/kubernetes/kubelet-plugins/volume/exec")
	provideDefault(&rc.config.Data, CONFIG_POOL_NAME, "pool")
	provideDefault(&rc.config.Data, CONFIG_LOG_ADDRESS, "")
	provideDefault(&rc.config.Data, CONFIG_MODE, CONFIG_MODE_LOCAL)
	provideDefault(&rc.config.Data, CONFIG_LOCAL_POOL_SIZE_PER_NODE, "10G")
	provideDefault(&rc.config.Data, CONFIG_LOCAL_POOL_LOCATION, "/var/lib/dotmesh")

	// TODO: listen for node_list_changed, dotmesh_pod_list_changed,
	// config_changed or pv_list_changed

	// TRACK NODES

	nodeIndexer, nodeInformer := cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(lo meta_v1.ListOptions) (runtime.Object, error) {
				// Add user-configurable selector to only care about certain nodes
				lo2 := lo.DeepCopy()
				if rc.config.Data[CONFIG_NODE_SELECTOR] != "" {
					lo2.LabelSelector = rc.config.Data[CONFIG_NODE_SELECTOR]
				}
				return client.Core().Nodes().List(*lo2)
			},
			WatchFunc: func(lo meta_v1.ListOptions) (watch.Interface, error) {
				// Add user-configurable selector to only care about certain nodes
				lo2 := lo.DeepCopy()
				if rc.config.Data[CONFIG_NODE_SELECTOR] != "" {
					lo2.LabelSelector = rc.config.Data[CONFIG_NODE_SELECTOR]
				}
				return client.Core().Nodes().Watch(*lo2)
			},
		},
		// The types of objects this informer will return
		&v1.Node{},
		// The resync period of this object. This will force a re-update of all
		// cached objects at this interval.  Every object will trigger the
		// `Updatefunc` even if there have been no actual updates triggered.
		60*time.Second,
		// Callback Functions to trigger on add/update/delete
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				glog.V(3).Info("NODE ADD %+v", obj)
				rc.scheduleUpdate()
			},
			UpdateFunc: func(old, new interface{}) {
				glog.V(3).Info("NODE UPDATE %+v -> %+v", old, new)
				rc.scheduleUpdate()
			},
			DeleteFunc: func(obj interface{}) {
				glog.V(3).Info("NODE DELETE %+v", obj)
				rc.scheduleUpdate()
			},
		},
		cache.Indexers{},
	)

	rc.nodeInformer = nodeInformer
	// NodeLister avoids some boilerplate code (e.g. convert runtime.Object to
	// *v1.node)
	rc.nodeLister = lister_v1.NewNodeLister(nodeIndexer)

	// TRACK DOTMESH PODS

	podIndexer, podInformer := cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(lo meta_v1.ListOptions) (runtime.Object, error) {
				// Add selectors to only list Dotmesh pods
				dmLo := lo.DeepCopy()
				dmLo.LabelSelector = fmt.Sprintf("%s=%s", DOTMESH_POD_ROLE_LABEL, DOTMESH_ROLE_SERVER)
				return client.Core().Pods(DOTMESH_NAMESPACE).List(*dmLo)
			},
			WatchFunc: func(lo meta_v1.ListOptions) (watch.Interface, error) {
				// Add selectors to only list Dotmesh pods
				dmLo := lo.DeepCopy()
				dmLo.LabelSelector = fmt.Sprintf("%s=%s", DOTMESH_POD_ROLE_LABEL, DOTMESH_ROLE_SERVER)
				return client.Core().Pods(DOTMESH_NAMESPACE).Watch(*dmLo)
			},
		},
		// The types of objects this informer will return
		&v1.Pod{},
		// The resync period of this object. This will force a re-update of all
		// cached objects at this interval.  Every object will trigger the
		// `Updatefunc` even if there have been no actual updates triggered.
		60*time.Second,
		// Callback Functions to trigger on add/update/delete
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				glog.V(3).Info("POD ADD %+v", obj)
				rc.scheduleUpdate()
			},
			UpdateFunc: func(old, new interface{}) {
				glog.V(3).Info("POD UPDATE %+v -> %+v", old, new)
				rc.scheduleUpdate()
			},
			DeleteFunc: func(obj interface{}) {
				glog.V(3).Info("POD DELETE %+v", obj)
				rc.scheduleUpdate()
			},
		},
		cache.Indexers{},
	)

	rc.podInformer = podInformer
	// PodLister avoids some boilerplate code (e.g. convert runtime.Object to
	// *v1.pod)
	rc.podLister = lister_v1.NewPodLister(podIndexer)

	return rc
}

func (c *dotmeshController) scheduleUpdate() {
	c.updatesNeededLock.Lock()
	defer c.updatesNeededLock.Unlock()

	c.updatesNeeded = true
}

func (c *dotmeshController) Run(stopCh chan struct{}) {
	glog.Infof("Starting Dotmesh Operator version %s", DOTMESH_VERSION)

	go c.nodeInformer.Run(stopCh)
	go c.podInformer.Run(stopCh)

	// Wait for all caches to be synced, before processing is started
	if !cache.WaitForCacheSync(stopCh, c.nodeInformer.HasSynced) {
		glog.Error(fmt.Errorf("Timed out waiting for node cache to sync"))
		return
	}

	if !cache.WaitForCacheSync(stopCh, c.podInformer.HasSynced) {
		glog.Error(fmt.Errorf("Timed out waiting for pod cache to sync"))
		return
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	glog.Info("Stopping Dotmesh Operator")
}

func (c *dotmeshController) runWorker() {
	needed :=
		func() bool {
			c.updatesNeededLock.Lock()
			defer c.updatesNeededLock.Unlock()
			needed := c.updatesNeeded
			c.updatesNeeded = false
			return needed
		}()

	if needed {
		err := c.process()
		if err != nil {
			glog.Error(err)
		}
	} else {
		time.Sleep(time.Second)
	}
}

func (c *dotmeshController) process() error {
	glog.V(1).Info("Analysing cluster status...")

	// EXAMINE NODES

	// nodes is a []*v1.Node
	// v1.Node is documented at https://godoc.org/k8s.io/api/core/v1#Node
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return err
	}

	// Ensure nodes are labelled correctly, so we can bind Dotmesh instances to them
	for _, node := range nodes {
		nodeName := node.ObjectMeta.Name
		labelName, ok := node.ObjectMeta.Labels[DOTMESH_NODE_LABEL]

		// We COULD use something other than the k8s node name as the
		// label name; if so, here is the place to do that.  The code
		// below all works in terms of the label name, so the change
		// need only be made here.  However, the k8s node name seems the
		// friendliest thing to use, as it saves making up another node
		// id.
		if !ok || labelName != nodeName {
			n2 := node.DeepCopy()
			n2.ObjectMeta.Labels[DOTMESH_NODE_LABEL] = nodeName
			glog.Infof("Labelling unfamiliar node %s so we can bind a Dotmesh to it", n2.ObjectMeta.Name)
			_, err := c.client.Core().Nodes().Update(n2)
			if err != nil {
				return err
			}
		}
	}

	// Set of all node IDs
	validNodes := map[string]struct{}{}

	// Set of node IDs, starts as all nodes but we strike out ones we
	// find a good dotmesh pod on
	undottedNodes := map[string]struct{}{}

	// Set of node IDs where starting new Dotmeshes is temporarily prohibited
	suspendedNodes := map[string]struct{}{}

	for _, node := range nodes {
		nodeLabel := node.ObjectMeta.Labels[DOTMESH_NODE_LABEL]
		glog.V(2).Infof("Observing node %s (labelled %s)", node.ObjectMeta.Name, nodeLabel)
		undottedNodes[nodeLabel] = struct{}{}
		validNodes[nodeLabel] = struct{}{}
	}

	// EXAMINE DOTMESH PODS

	// dotmeshes is a []*v1.Pod
	// v1.Pod is documented at https://godoc.org/k8s.io/api/core/v1#Pod
	dotmeshes, err := c.podLister.List(labels.Everything())
	if err != nil {
		return err
	}

	dotmeshesToKill := map[string]struct{}{} // Set of pod IDs of dotmesh pods that need to die
	dotmeshIsRunning := map[string]bool{}    // Set of pod IDs that are in the "Running" state

	runningPodCount := 0

	for _, dotmesh := range dotmeshes {
		podName := dotmesh.ObjectMeta.Name
		status := dotmesh.Status.Phase

		dotmeshIsRunning[podName] = status == v1.PodRunning
		if status == v1.PodRunning {
			runningPodCount++
		}

		// Find the node this pod is bound to, as if it's starting up it
		// won't actually be scheduled onto that node yet
		boundNode, ok := dotmesh.Spec.NodeSelector[DOTMESH_NODE_LABEL]
		if !ok {
			glog.Infof("Observing pod %s - cannot find %s label", podName, DOTMESH_NODE_LABEL)
			// Weird and strange, mark it for death
			dotmeshesToKill[podName] = struct{}{}
			continue
		}

		// Find the image this pod is running
		if len(dotmesh.Spec.Containers) != 1 {
			glog.Infof("Observing pod %s - it has %d containers, should be 1", len(dotmesh.Spec.Containers))
			// Weird and strange, mark it for death
			dotmeshesToKill[podName] = struct{}{}
			// But don't try starting any new dotmesh on the node it's SUPPOSED to be on until it's gone
			suspendedNodes[boundNode] = struct{}{}
			continue
		}

		image := dotmesh.Spec.Containers[0].Image
		if image != DOTMESH_IMAGE {
			// Wrong image, mark it for death
			glog.V(2).Infof("Observing pod %s running wrong image %s (should be %s)", podName, image, DOTMESH_IMAGE)
			dotmeshesToKill[podName] = struct{}{}
			// But don't try starting any new dotmesh on the node it's SUPPOSED to be on until it's gone
			suspendedNodes[boundNode] = struct{}{}
			continue
		}

		runningNode := dotmesh.Spec.NodeName
		// This is not set if the pod isn't running yet
		if runningNode != "" {
			if runningNode != boundNode {
				glog.Infof("Observing pod %s - running on node %s but bound to node %s", podName, runningNode, boundNode)
				// Weird and strange, mark it for death
				dotmeshesToKill[podName] = struct{}{}

				// But don't try starting any new dotmesh on the node it's SUPPOSED to be on until it's gone
				suspendedNodes[boundNode] = struct{}{}

				continue
			}
		}

		_, nodeOk := validNodes[boundNode]
		if !nodeOk {
			glog.Infof("Observing pod %s - bound to invalid node %s", podName, boundNode)
			// Weird and strange, mark it for death
			dotmeshesToKill[podName] = struct{}{}
			continue
		}

		if status == v1.PodFailed {
			// We're deleting the pod, so the user can't "kubectl describe" it, so let's log lots of stuff
			glog.Infof("Observing pod %s - FAILED (Message: %s) (Reason: %s)",
				podName,
				dotmesh.Status.Message,
				dotmesh.Status.Reason,
			)
			for _, cond := range dotmesh.Status.Conditions {
				glog.Infof("Failed pod %s - condition %+v", podName, cond)
			}
			for _, cont := range dotmesh.Status.ContainerStatuses {
				glog.Infof("Failed pod %s - container %+v", podName, cont)
			}

			// Broken, mark it for death
			dotmeshesToKill[podName] = struct{}{}

			// But don't try starting a new dotmesh on there until it's gone
			suspendedNodes[boundNode] = struct{}{}
			continue
		}

		// At this point, we believe this is a valid running Dotmesh pod.
		// That node has a dotmesh, so isn't undotted.
		glog.V(2).Infof("Observing pod %s running %s on %s (status: %s)", podName, image, boundNode, dotmesh.Status.Phase)
		delete(undottedNodes, boundNode)
	}

	dottedNodeCount := len(validNodes) - len(undottedNodes)
	glog.V(1).Infof("%d healthy-looking dotmeshes exist to run on %d nodes; %d of them seem to be actually running; %d dotmeshes need deleting, and %d out of %d undotted nodes are temporarily suspended",
		dottedNodeCount, len(validNodes),
		runningPodCount,
		len(dotmeshesToKill),
		len(suspendedNodes), len(undottedNodes))

	// DELETE UNWANTED DOTMESH PODS

	// Pods in dotmeshesToKill may be running an old version or on the
	// wrong node or something, so may still be contributing to the
	// cluster if dotmeshIsRunning[id] is true.

	// We need to rate-limit the killing of running pods, to implement
	// a nice rolling restart!

	// We want to keep at least CLUSTER_MINIMUM_RATIO * len(validNodes)
	// pods running. We have runningPodCount nodes running, as far as
	// we can tell, so let's never delete so many in one go as to go
	// below the minimum number of pods running.

	// The one case to be careful of is if scheduled pods aren't
	// starting, so we hold back on deleting existing pods to wait for
	// them forever, when if we deleted those pods new ones could
	// spring up in their wake...

	clusterMinimumPopulation := int(CLUSTER_MINIMUM_RATIO * float32(len(validNodes)))
	clusterPopulation := runningPodCount

	glog.V(1).Infof("%d/%d nodes might just be running or getting there, minimum target is %d",
		clusterPopulation, len(validNodes),
		clusterMinimumPopulation)

	for dotmeshName, _ := range dotmeshesToKill {
		if !dotmeshIsRunning[dotmeshName] || clusterPopulation > clusterMinimumPopulation {
			glog.Infof("Deleting pod %s", dotmeshName)
			dp := meta_v1.DeletePropagationBackground
			err = c.client.Core().Pods(DOTMESH_NAMESPACE).Delete(dotmeshName, &meta_v1.DeleteOptions{
				PropagationPolicy: &dp,
			})
			if err != nil {
				// Do not abort in error case, just keep pressing on
				glog.Error(err)
			}

			if dotmeshIsRunning[dotmeshName] {
				// We're killing a running pod
				clusterPopulation--
			}
		} else {
			glog.Infof("Sparing pod %s to rate-limit the deletion of running pods", dotmeshName)
		}
	}

	/* TODO: Analyse available PVs
	pvs = list_of_dotmesh_pvs_in_az()
	unused_pvs = pvs - map(pv_of_dotmesh, dotmesh_nodes)
	*/

	// CREATE NEW DOTMESH PODS WHERE NEEDED

	for node, _ := range undottedNodes {
		_, suspended := suspendedNodes[node]
		if suspended {
			glog.Infof("Not creating a pod on undotted node %s, as the old pod is being cleared up", node)
			continue
		}

		/* TODO
		if config.use_pv_storage:
			if len(unused_pvs) == 0:
				unused_pvs.push(make_new_pv(config.new_pv_size))
			storage = unused_pvs.pop()
		else:
		*/

		// Create a dotmesh pod (with local storage for now) assigned to this node
		privileged := true
		newDotmesh := v1.Pod{
			ObjectMeta: meta_v1.ObjectMeta{
				Name:      fmt.Sprintf("dotmesh-%s", node),
				Namespace: "dotmesh",
				Labels: map[string]string{
					DOTMESH_POD_ROLE_LABEL: DOTMESH_ROLE_SERVER,
				},
				Annotations: map[string]string{},
			},
			Spec: v1.PodSpec{
				HostPID: true,
				// This is what binds the pod to a specific node
				NodeSelector: map[string]string{
					DOTMESH_NODE_LABEL: node,
				},
				Tolerations: []v1.Toleration{
					v1.Toleration{
						Effect:   v1.TaintEffectNoSchedule,
						Operator: v1.TolerationOpExists,
					}},
				InitContainers: []v1.Container{},
				Containers: []v1.Container{
					v1.Container{
						Name:  "dotmesh-outer",
						Image: DOTMESH_IMAGE,
						Command: []string{
							"/require_zfs.sh",
							"dotmesh-server",
						},
						SecurityContext: &v1.SecurityContext{
							Privileged: &privileged,
						},
						Ports: []v1.ContainerPort{
							{
								Name:          "dotmesh-api",
								ContainerPort: int32(32607),
								Protocol:      v1.ProtocolTCP,
							},
						},
						VolumeMounts: []v1.VolumeMount{
							{Name: "docker-sock", MountPath: "/var/run/docker.sock"},
							{Name: "run-docker", MountPath: "/run/docker"},
							{Name: "var-lib", MountPath: "/var/lib"},
							{Name: "system-lib", MountPath: "/system-lib/lib"},
							{Name: "dotmesh-kernel-modules", MountPath: "/bundled-lib"},
							{Name: "dotmesh-secret", MountPath: "/secret"},
							{Name: "test-pools-dir", MountPath: "/dotmesh-test-pools"},
						},
						Env: []v1.EnvVar{
							{Name: "HOSTNAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "spec.nodeName"}}},
							{Name: "DOTMESH_ETCD_ENDPOINT", Value: "http://dotmesh-etcd-cluster-client.dotmesh.svc.cluster.local:2379"},
							{Name: "DOTMESH_DOCKER_IMAGE", Value: DOTMESH_IMAGE},
							{Name: "PATH", Value: "/bundled-lib/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
							{Name: "LD_LIBRARY_PATH", Value: "/bundled-lib/lib:/bundled-lib/usr/lib/"},
							{Name: "ALLOW_PUBLIC_REGISTRATION", Value: "1"},
							{Name: "INITIAL_ADMIN_PASSWORD_FILE", Value: "/secret/dotmesh-admin-password.txt"},
							{Name: "INITIAL_ADMIN_API_KEY_FILE", Value: "/secret/dotmesh-api-key.txt"},
							{Name: "USE_POOL_NAME", Value: c.config.Data[CONFIG_POOL_NAME]},
							{Name: "USE_POOL_DIR", Value: c.config.Data[CONFIG_LOCAL_POOL_LOCATION]},
							{Name: "POOL_SIZE", Value: c.config.Data[CONFIG_LOCAL_POOL_SIZE_PER_NODE]},
							{Name: "LOG_ADDR", Value: c.config.Data[CONFIG_LOG_ADDRESS]},
							{Name: "DOTMESH_UPGRADES_URL", Value: c.config.Data[CONFIG_UPGRADES_URL]},
							{Name: "DOTMESH_UPGRADES_INTERVAL_SECONDS", Value: c.config.Data[CONFIG_UPGRADES_INTERVAL_SECONDS]},
							{Name: "FLEXVOLUME_DRIVER_DIR", Value: c.config.Data[CONFIG_FLEXVOLUME_DRIVER_DIR]},
						},
						ImagePullPolicy: v1.PullAlways,
						LivenessProbe: &v1.Probe{
							Handler: v1.Handler{
								HTTPGet: &v1.HTTPGetAction{
									Path: "/status",
									Port: intstr.FromInt(32607),
								},
							},
							InitialDelaySeconds: int32(30),
						},
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU: resource.MustParse("10m"),
							},
						},
					},
				},
				RestartPolicy:      v1.RestartPolicyNever,
				ServiceAccountName: "dotmesh",
				Volumes: []v1.Volume{
					{Name: "test-pools-dir", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/dotmesh-test-pools"}}},
					{Name: "run-docker", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/var/run/docker"}}},
					{Name: "docker-sock", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/var/run/docker.sock"}}},
					{Name: "var-lib", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/var/lib"}}},
					{Name: "system-lib", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/lib"}}},
					{Name: "dotmesh-kernel-modules", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
					{Name: "dotmesh-secret", VolumeSource: v1.VolumeSource{Secret: &v1.SecretVolumeSource{SecretName: "dotmesh"}}},
				},
			},
		}
		glog.Infof("Creating pod %s running %s on node %s", newDotmesh.ObjectMeta.Name, DOTMESH_IMAGE, node)
		_, err = c.client.Core().Pods(DOTMESH_NAMESPACE).Create(&newDotmesh)
		if err != nil {
			// Do not abort in error case, just keep pressing on
			glog.Error(err)
		}
	}

	return nil
}
