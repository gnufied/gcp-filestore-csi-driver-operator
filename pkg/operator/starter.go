package operator

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	configlisters "github.com/openshift/client-go/config/listers/config/v1"
	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned"
	operatorinformer "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/gcp-filestore-csi-driver-operator/assets"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivernodeservicecontroller"
	dc "github.com/openshift/library-go/pkg/operator/deploymentcontroller"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	// FIXME: this is temporary. We need to move this to library-go.
	"github.com/openshift/gcp-filestore-csi-driver-operator/pkg/operator/staticresources"
)

const (
	// Operand and operator run in the same namespace
	operatorName          = "gcp-filestore-csi-driver-operator"
	operandName           = "gcp-filestore-csi-driver"
	cloudCredSecretName   = "gcp-filestore-cloud-credentials"
	metricsCertSecretName = "gcp-filestore-csi-driver-controller-metrics-serving-cert"
	trustedCAConfigMap    = "gcp-filestore-csi-driver-trusted-ca-bundle"

	namespaceReplaceKey = "${NAMESPACE}"

	// globalInfrastructureName is the default name of the Infrastructure object
	globalInfrastructureName = "cluster"

	// ocpDefaultLabelFmt is the format string for the default label
	// added to the OpenShift created GCP resources.
	ocpDefaultLabelFmt = "kubernetes-io-cluster-%s=owned"
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	operatorNamespace := controllerConfig.OperatorNamespace

	// Create core clientset and informers
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, operatorNamespace, "")
	secretInformer := kubeInformersForNamespaces.InformersFor(operatorNamespace).Core().V1().Secrets()
	configMapInformer := kubeInformersForNamespaces.InformersFor(operatorNamespace).Core().V1().ConfigMaps()
	nodeInformer := kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes()
	typedVersionedClient := operatorv1client.NewForConfigOrDie(controllerConfig.KubeConfig)
	operatorInformer := operatorinformer.NewSharedInformerFactory(typedVersionedClient, 20*time.Minute)

	// Create config clientset and informer. This is used to get the cluster ID
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	configInformers := configinformers.NewSharedInformerFactory(configClient, 20*time.Minute)
	infraInformer := configInformers.Config().V1().Infrastructures()

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := opv1.SchemeGroupVersion.WithResource("clustercsidrivers")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(controllerConfig.KubeConfig, gvr, string(opv1.GCPFilestoreCSIDriver))
	if err != nil {
		return err
	}

	// Create apiextension client. This is used to verify is a VolumeSnapshotClass CRD exists.
	apiExtClient, err := apiextclient.NewForConfig(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	csiControllerSet := csicontrollerset.NewCSIControllerSet(
		operatorClient,
		controllerConfig.EventRecorder,
	).WithLogLevelController().WithManagementStateController(
		operandName,
		true, // Set this operator as removable
	).WithCSIConfigObserverController(
		"GCPFilestoreDriverCSIConfigObserverController",
		configInformers,
	).WithCSIDriverControllerService(
		"GCPFilestoreDriverControllerServiceController",
		replaceNamespaceFunc(operatorNamespace),
		"controller.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorNamespace),
		configInformers,
		[]factory.Informer{
			nodeInformer.Informer(),
			infraInformer.Informer(),
			secretInformer.Informer(),
			configMapInformer.Informer(),
		},
		csidrivercontrollerservicecontroller.WithObservedProxyDeploymentHook(),
		csidrivercontrollerservicecontroller.WithCABundleDeploymentHook(
			operatorNamespace,
			trustedCAConfigMap,
			configMapInformer,
		),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			operatorNamespace,
			cloudCredSecretName,
			secretInformer,
		),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			operatorNamespace,
			metricsCertSecretName,
			secretInformer,
		),
		csidrivercontrollerservicecontroller.WithReplicasHook(nodeInformer.Lister()),
		withCustomLabels(infraInformer.Lister()),
	).WithCSIDriverNodeService(
		"GCPFilestoreDriverNodeServiceController",
		replaceNamespaceFunc(operatorNamespace),
		"node.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorNamespace),
		[]factory.Informer{configMapInformer.Informer()},
		csidrivernodeservicecontroller.WithObservedProxyDaemonSetHook(),
		csidrivernodeservicecontroller.WithCABundleDaemonSetHook(
			operatorNamespace,
			trustedCAConfigMap,
			configMapInformer,
		),
	).WithCredentialsRequestController(
		"GCPFilestoreDriverCredentialsRequestController",
		operatorNamespace,
		replaceNamespaceFunc(operatorNamespace),
		"credentials.yaml",
		dynamicClient,
		operatorInformer,
	).WithServiceMonitorController(
		"GCPFilestoreDriverServiceMonitorController",
		dynamicClient,
		replaceNamespaceFunc(operatorNamespace),
		"servicemonitor.yaml",
	)

	objsToSync := staticresources.SyncObjects{
		CSIDriver:                resourceread.ReadCSIDriverV1OrDie(mustReplaceNamespace(operatorNamespace, "csidriver.yaml")),
		PrivilegedRole:           resourceread.ReadClusterRoleV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/privileged_role.yaml")),
		NodeServiceAccount:       resourceread.ReadServiceAccountV1OrDie(mustReplaceNamespace(operatorNamespace, "node_sa.yaml")),
		NodeRoleBinding:          resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/node_privileged_binding.yaml")),
		ControllerServiceAccount: resourceread.ReadServiceAccountV1OrDie(mustReplaceNamespace(operatorNamespace, "controller_sa.yaml")),
		ControllerRoleBinding:    resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/controller_privileged_binding.yaml")),
		ProvisionerRoleBinding:   resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/main_provisioner_binding.yaml")),
		VolumesnapshotReaderProvisionerRoleBinding: resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/volumesnapshot_reader_provisioner_binding.yaml")),
		ResizerRoleBinding:                         resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/main_resizer_binding.yaml")),
		StorageclassReaderResizerRoleBinding:       resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/storageclass_reader_resizer_binding.yaml")),
		SnapshotterRoleBinding:                     resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/main_snapshotter_binding.yaml")),
		VolumeSnapshotClass:                        resourceread.ReadUnstructuredOrDie(mustReplaceNamespace(operatorNamespace, "volumesnapshotclass.yaml")),
		ControllerPDB:                              resourceread.ReadPodDisruptionBudgetV1OrDie(mustReplaceNamespace(operatorNamespace, "controller_pdb.yaml")),
		PrometheusRole:                             resourceread.ReadRoleV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/prometheus_role.yaml")),
		PrometheusRoleBinding:                      resourceread.ReadRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/prometheus_rolebinding.yaml")),
		LeaseLeaderElectionRole:                    resourceread.ReadRoleV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/lease_leader_election_role.yaml")),
		LeaseLeaderElectionRoleBinding:             resourceread.ReadRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/lease_leader_election_rolebinding.yaml")),
		MetricsService:                             resourceread.ReadServiceV1OrDie(mustReplaceNamespace(operatorNamespace, "service.yaml")),
		RBACProxyRole:                              resourceread.ReadClusterRoleV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/kube_rbac_proxy_role.yaml")),
		RBACProxyRoleBinding:                       resourceread.ReadClusterRoleBindingV1OrDie(mustReplaceNamespace(operatorNamespace, "rbac/kube_rbac_proxy_binding.yaml")),
		CAConfigMap:                                resourceread.ReadConfigMapV1OrDie(mustReplaceNamespace(operatorNamespace, "cabundle_cm.yaml")),
	}
	staticController := staticresources.NewCSIStaticResourceController(
		"GCPFilestoreDriverCSIStaticResourceController",
		operatorNamespace,
		operatorClient,
		kubeClient,
		apiExtClient,
		dynamicClient,
		kubeInformersForNamespaces,
		controllerConfig.EventRecorder,
		objsToSync,
	)

	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())
	go configInformers.Start(ctx.Done())
	go operatorInformer.Start(ctx.Done())

	klog.Info("Starting controllerset")
	go csiControllerSet.Run(ctx, 1)
	go staticController.Run(ctx, 1)

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

func mustReplaceNamespace(namespace, file string) []byte {
	content, err := assets.ReadFile(file)
	if err != nil {
		panic(err)
	}
	return bytes.ReplaceAll(content, []byte(namespaceReplaceKey), []byte(namespace))
}

func replaceNamespaceFunc(namespace string) resourceapply.AssetFunc {
	return func(name string) ([]byte, error) {
		content, err := assets.ReadFile(name)
		if err != nil {
			panic(err)
		}
		return bytes.ReplaceAll(content, []byte(namespaceReplaceKey), []byte(namespace)), nil
	}
}

// withCustomLabels adds labels from Infrastructure.Status.PlatformStatus.GCP.ResourceLabels to the
// driver command line as --extra-labels=<key1>=<value1>,<key2>=<value2>,...
func withCustomLabels(infraLister configlisters.InfrastructureLister) dc.DeploymentHookFunc {
	return func(spec *opv1.OperatorSpec, deployment *appsv1.Deployment) error {
		infra, err := infraLister.Get(globalInfrastructureName)
		if err != nil {
			return fmt.Errorf("custom labels: failed to fetch global Infrastructure object: %w", err)
		}

		var labels []string
		if infra.Status.PlatformStatus != nil &&
			infra.Status.PlatformStatus.GCP != nil &&
			infra.Status.PlatformStatus.GCP.ResourceLabels != nil {
			labels = make([]string, len(infra.Status.PlatformStatus.GCP.ResourceLabels))
			for i, label := range infra.Status.PlatformStatus.GCP.ResourceLabels {
				labels[i] = fmt.Sprintf("%s=%s", label.Key, label.Value)
			}
		}

		labels = append(labels, fmt.Sprintf(ocpDefaultLabelFmt, infra.Status.InfrastructureName))
		labelsStr := strings.Join(labels, ",")
		labelsArg := fmt.Sprintf("--extra-labels=%s", labelsStr)
		klog.V(1).Infof("withCustomLabels: adding extra-labels arg to driver with value %s", labelsStr)

		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]
			if container.Name != "csi-driver" {
				continue
			}
			container.Args = append(container.Args, labelsArg)
		}
		return nil
	}
}
